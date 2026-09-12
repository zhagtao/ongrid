// Package store implements LLM Wiki persistence with GORM.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"gorm.io/gorm"
)

type Repo struct{ db *gorm.DB }

func New(db *gorm.DB) *Repo { return &Repo{db: db} }

func Migrate(db *gorm.DB) error {
	if db == nil {
		return errors.New("migrate llm wiki: database is required")
	}
	if db.Dialector.Name() != "sqlite" {
		return fmt.Errorf("migrate llm wiki: sqlite is required, got %s", db.Dialector.Name())
	}
	for _, statement := range []string{"PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL"} {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("migrate llm wiki: configure sqlite: %w", err)
		}
	}
	if err := db.AutoMigrate(&model.Source{}, &model.SourceVersion{}, &model.SourceChunk{}, &model.CompileJob{}, &model.Topic{}, &model.TopicEvidence{}, &model.Page{}, &model.PageSource{}, &model.PageRelation{}); err != nil {
		return fmt.Errorf("migrate llm wiki: state and catalog tables: %w", err)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS wiki_vectors (tenant_id INTEGER NOT NULL, page_id TEXT NOT NULL, body_hash TEXT NOT NULL, dimension INTEGER NOT NULL, vector BLOB NOT NULL, PRIMARY KEY (tenant_id, page_id))`,
		`CREATE TABLE IF NOT EXISTS wiki_index_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS wiki_fts USING fts5(page_key UNINDEXED, tenant_id UNINDEXED, page_id UNINDEXED, page_type UNINDEXED, title, aliases, content, tokenize='trigram')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("migrate llm wiki: derived index tables: %w", err)
		}
	}
	return nil
}

func (r *Repo) UpsertSourceVersion(ctx context.Context, source *model.Source, version *model.SourceVersion) (*model.Source, *model.SourceVersion, bool, error) {
	var resultSource model.Source
	var resultVersion model.SourceVersion
	changed := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Where("tenant_id = ? AND source_key = ? AND deleted_at IS NULL", source.TenantID, source.SourceKey).First(&resultSource).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resultSource = *source
			if err := tx.Create(&resultSource).Error; err != nil {
				return fmt.Errorf("create wiki source: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("get wiki source: %w", err)
		}
		if resultSource.ContentSHA256 == version.SHA256 && resultSource.CurrentVersionID != nil {
			if err := tx.Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", source.TenantID, *resultSource.CurrentVersionID).First(&resultVersion).Error; err != nil {
				return fmt.Errorf("get current wiki version: %w", err)
			}
			return nil
		}
		changed = true
		version.TenantID = source.TenantID
		version.SourceID = resultSource.ID
		if err := tx.Where("tenant_id = ? AND source_id = ? AND sha256 = ? AND deleted_at IS NULL", source.TenantID, resultSource.ID, version.SHA256).First(&resultVersion).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			resultVersion = *version
			if err := tx.Create(&resultVersion).Error; err != nil {
				return fmt.Errorf("create wiki version: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("get wiki version: %w", err)
		}
		status := model.SourcePending
		if resultSource.CurrentVersionID != nil {
			status = model.SourceStale
		}
		updates := map[string]any{"source_type": source.SourceType, "raw_path": source.RawPath, "current_version_id": resultVersion.ID, "content_sha256": version.SHA256, "status": status}
		if err := tx.Model(&model.Source{}).Where("tenant_id = ? AND id = ?", source.TenantID, resultSource.ID).Updates(updates).Error; err != nil {
			return fmt.Errorf("update wiki source: %w", err)
		}
		resultSource.SourceType = source.SourceType
		resultSource.RawPath = source.RawPath
		resultSource.CurrentVersionID = &resultVersion.ID
		resultSource.ContentSHA256 = version.SHA256
		resultSource.Status = status
		return nil
	})
	return &resultSource, &resultVersion, changed, err
}

func (r *Repo) ListSources(ctx context.Context, tenantID uint64, status string, limit int) ([]*model.Source, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	q := r.db.WithContext(ctx).Model(&model.Source{}).Where("tenant_id = ? AND deleted_at IS NULL", tenantID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count wiki sources: %w", err)
	}
	var rows []*model.Source
	if err := q.Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("list wiki sources: %w", err)
	}
	return rows, total, nil
}

func (r *Repo) GetSource(ctx context.Context, tenantID, id uint64) (*model.Source, error) {
	var row model.Source
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, id).First(&row).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &row, nil
}

func (r *Repo) DeleteSource(ctx context.Context, tenantID, id uint64) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var source model.Source
		if err := tx.Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, id).First(&source).Error; err != nil {
			return mapNotFound(err)
		}
		if source.ActiveJobID != nil {
			return errors.Join(errs.ErrConflict, fmt.Errorf("wiki source %d has an active compile job", id))
		}

		var versionIDs []uint64
		if err := tx.Model(&model.SourceVersion{}).
			Where("tenant_id = ? AND source_id = ? AND deleted_at IS NULL", tenantID, id).
			Pluck("id", &versionIDs).Error; err != nil {
			return fmt.Errorf("list wiki source versions for delete: %w", err)
		}
		var pageIDs []string
		if len(versionIDs) > 0 {
			if err := tx.Model(&model.PageSource{}).
				Where("tenant_id = ? AND source_version_id IN ? AND deleted_at IS NULL", tenantID, versionIDs).
				Distinct().Pluck("page_id", &pageIDs).Error; err != nil {
				return fmt.Errorf("list wiki pages for source delete: %w", err)
			}
		}
		var topicIDs []string
		if err := tx.Model(&model.TopicEvidence{}).
			Where("tenant_id = ? AND source_id = ? AND deleted_at IS NULL", tenantID, id).
			Distinct().Pluck("topic_id", &topicIDs).Error; err != nil {
			return fmt.Errorf("list wiki topics for source delete: %w", err)
		}
		pageIDs = append(pageIDs, topicIDs...)
		pageIDs = dedupeStrings(pageIDs)
		if err := tx.Where("tenant_id = ? AND source_id = ?", tenantID, id).Delete(&model.TopicEvidence{}).Error; err != nil {
			return fmt.Errorf("delete wiki source evidence: %w", err)
		}
		for _, topicID := range topicIDs {
			var remaining int64
			if err := tx.Model(&model.TopicEvidence{}).Where("tenant_id = ? AND topic_id = ? AND deleted_at IS NULL", tenantID, topicID).Count(&remaining).Error; err != nil {
				return fmt.Errorf("count remaining wiki topic evidence: %w", err)
			}
			if remaining == 0 {
				if err := tx.Unscoped().Where("tenant_id = ? AND topic_id = ?", tenantID, topicID).Delete(&model.Topic{}).Error; err != nil {
					return fmt.Errorf("delete orphan wiki topic: %w", err)
				}
				continue
			}
			if err := tx.Model(&model.Topic{}).Where("tenant_id = ? AND topic_id = ?", tenantID, topicID).Updates(map[string]any{"status": model.TopicPending, "generation_fingerprint": ""}).Error; err != nil {
				return fmt.Errorf("unpublish affected wiki topic: %w", err)
			}
		}
		// Page materializations that referenced the deleted source are stale,
		// including shared topics that still retain evidence from other sources.
		// Keep the canonical Topic metadata in the latter case, marked pending,
		// so a later compile can rewrite it from the remaining evidence.
		if len(pageIDs) > 0 {
			if err := tx.Where("tenant_id = ? AND (from_page_id IN ? OR to_page_id IN ?)", tenantID, pageIDs, pageIDs).Delete(&model.PageRelation{}).Error; err != nil {
				return fmt.Errorf("delete wiki page relations: %w", err)
			}
			if err := tx.Where("tenant_id = ? AND page_id IN ?", tenantID, pageIDs).Delete(&model.PageSource{}).Error; err != nil {
				return fmt.Errorf("delete wiki page sources: %w", err)
			}
			if err := tx.Where("tenant_id = ? AND page_id IN ?", tenantID, pageIDs).Delete(&model.Page{}).Error; err != nil {
				return fmt.Errorf("delete wiki pages: %w", err)
			}
		}
		if len(versionIDs) > 0 {
			if err := tx.Where("tenant_id = ? AND version_id IN ?", tenantID, versionIDs).Delete(&model.SourceChunk{}).Error; err != nil {
				return fmt.Errorf("delete wiki source chunks: %w", err)
			}
		}
		if err := tx.Where("tenant_id = ? AND source_id = ?", tenantID, id).Delete(&model.SourceVersion{}).Error; err != nil {
			return fmt.Errorf("delete wiki source versions: %w", err)
		}
		if err := tx.Where("tenant_id = ? AND id = ?", tenantID, id).Delete(&model.Source{}).Error; err != nil {
			return fmt.Errorf("delete wiki source: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete wiki source %d: %w", id, err)
	}
	return nil
}

func (r *Repo) DeletePage(ctx context.Context, tenantID uint64, pageID string) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var page model.Page
		if err := tx.Where("tenant_id = ? AND page_id = ? AND deleted_at IS NULL", tenantID, pageID).First(&page).Error; err != nil {
			return mapNotFound(err)
		}
		if err := tx.Where("tenant_id = ? AND (from_page_id = ? OR to_page_id = ?)", tenantID, pageID, pageID).Delete(&model.PageRelation{}).Error; err != nil {
			return fmt.Errorf("delete wiki page relations: %w", err)
		}
		if err := tx.Where("tenant_id = ? AND page_id = ?", tenantID, pageID).Delete(&model.PageSource{}).Error; err != nil {
			return fmt.Errorf("delete wiki page sources: %w", err)
		}
		if err := tx.Where("tenant_id = ? AND page_id = ?", tenantID, pageID).Delete(&model.Page{}).Error; err != nil {
			return fmt.Errorf("delete wiki page: %w", err)
		}
		if err := tx.Model(&model.Topic{}).Where("tenant_id = ? AND topic_id = ?", tenantID, pageID).Updates(map[string]any{"status": model.TopicPending, "generation_fingerprint": ""}).Error; err != nil {
			return fmt.Errorf("unpublish wiki topic: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete wiki page %q: %w", pageID, err)
	}
	return nil
}

func (r *Repo) GetVersion(ctx context.Context, tenantID, id uint64) (*model.SourceVersion, error) {
	var row model.SourceVersion
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, id).First(&row).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &row, nil
}

func (r *Repo) MarkVersionSchema(ctx context.Context, tenantID, id uint64, schemaVersion string) error {
	res := r.db.WithContext(ctx).Model(&model.SourceVersion{}).Where("tenant_id = ? AND id = ?", tenantID, id).Update("schema_version", schemaVersion)
	if res.Error != nil {
		return fmt.Errorf("mark wiki version schema: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

func (r *Repo) CreateJob(ctx context.Context, job *model.CompileJob) error {
	var active int64
	if err := r.db.WithContext(ctx).Model(&model.CompileJob{}).Where("tenant_id = ? AND source_ids_json = ? AND status IN ? AND deleted_at IS NULL", job.TenantID, job.SourceIDsJSON, []string{model.JobPending, model.JobRunning}).Count(&active).Error; err != nil {
		return fmt.Errorf("check active wiki job: %w", err)
	}
	if active > 0 {
		return errors.Join(errs.ErrConflict, errors.New("an active compile job already covers these sources"))
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(job).Error; err != nil {
			return err
		}
		return lockJobSources(tx, job)
	})
	if err != nil {
		if isDuplicateKey(err) {
			return errors.Join(errs.ErrConflict, errors.New("an active compile job already covers these sources"))
		}
		if errors.Is(err, errs.ErrConflict) {
			return err
		}
		return fmt.Errorf("create wiki job: %w", err)
	}
	return nil
}

func (r *Repo) ListJobs(ctx context.Context, tenantID uint64, limit int) ([]*model.CompileJob, int64, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := r.db.WithContext(ctx).Model(&model.CompileJob{}).Where("tenant_id = ? AND deleted_at IS NULL", tenantID)
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count wiki jobs: %w", err)
	}
	var rows []*model.CompileJob
	if err := q.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("list wiki jobs: %w", err)
	}
	return rows, total, nil
}

func (r *Repo) GetJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error) {
	var row model.CompileJob
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL", tenantID, id).First(&row).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &row, nil
}

func (r *Repo) CountActiveJobs(ctx context.Context, tenantID uint64) (int64, error) {
	var count int64
	if err := r.db.WithContext(ctx).Model(&model.CompileJob{}).Where("tenant_id = ? AND status IN ? AND deleted_at IS NULL", tenantID, []string{model.JobPending, model.JobRunning}).Count(&count).Error; err != nil {
		return 0, fmt.Errorf("count active wiki jobs: %w", err)
	}
	return count, nil
}

func (r *Repo) RetryJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error) {
	job, err := r.GetJob(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(append([]byte(fmt.Sprintf("%d:", tenantID)), []byte(job.SourceIDsJSON)...))
	activeKey := hex.EncodeToString(digest[:])
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.CompileJob{}).Where("tenant_id = ? AND id = ? AND status IN ?", tenantID, id, []string{model.JobFailed, model.JobCancelled}).Updates(map[string]any{"status": model.JobPending, "stage": "queued", "cancel_requested": false, "error_message": "", "lease_owner": "", "lease_expires_at": nil, "active_key": activeKey})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errs.ErrInvalid
		}
		job.ActiveKey = &activeKey
		return lockJobSources(tx, job)
	})
	if err != nil {
		if isDuplicateKey(err) || errors.Is(err, errs.ErrConflict) {
			return nil, errors.Join(errs.ErrConflict, errors.New("an active compile job already covers these sources"))
		}
		return nil, fmt.Errorf("retry wiki job: %w", err)
	}
	return r.GetJob(ctx, tenantID, id)
}

func (r *Repo) CancelJob(ctx context.Context, tenantID, id uint64) (*model.CompileJob, error) {
	job, err := r.GetJob(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}
	if job.Status != model.JobPending && job.Status != model.JobRunning {
		return nil, errs.ErrInvalid
	}
	updates := map[string]any{"cancel_requested": true, "status": model.JobCancelled, "active_key": nil, "lease_owner": "", "lease_expires_at": nil}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.CompileJob{}).Where("tenant_id = ? AND id = ? AND status = ?", tenantID, id, job.Status).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errs.ErrInvalid
		}
		if err := unlockJobSources(tx, tenantID, id); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cancel wiki job: %w", err)
	}
	return r.GetJob(ctx, tenantID, id)
}

func (r *Repo) ClaimJob(ctx context.Context, tenantID uint64, owner string, lease time.Duration) (*model.CompileJob, error) {
	now := time.Now().UTC()
	var candidates []model.CompileJob
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL AND cancel_requested = ? AND status IN ?", tenantID, false, []string{model.JobPending, model.JobRunning}).Order("id ASC").Limit(32).Find(&candidates).Error; err != nil {
		return nil, fmt.Errorf("find wiki job: %w", err)
	}
	var candidate *model.CompileJob
	for i := range candidates {
		if candidates[i].Status == model.JobPending || (candidates[i].LeaseExpiresAt != nil && candidates[i].LeaseExpiresAt.Before(now)) {
			candidate = &candidates[i]
			break
		}
	}
	if candidate == nil {
		return nil, biz.ErrNoPendingJob
	}
	expires := now.Add(lease)
	claim := r.db.WithContext(ctx).Model(&model.CompileJob{}).Where("tenant_id = ? AND id = ? AND cancel_requested = ?", tenantID, candidate.ID, false)
	if candidate.Status == model.JobPending {
		claim = claim.Where("status = ?", model.JobPending)
	} else {
		claim = claim.Where("status = ? AND lease_owner = ? AND lease_expires_at = ?", model.JobRunning, candidate.LeaseOwner, candidate.LeaseExpiresAt)
	}
	res := claim.Updates(map[string]any{"status": model.JobRunning, "stage": "claimed", "lease_owner": owner, "lease_expires_at": expires, "attempt": gorm.Expr("attempt + 1")})
	if res.Error != nil {
		return nil, fmt.Errorf("claim wiki job: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, biz.ErrNoPendingJob
	}
	return r.GetJob(ctx, tenantID, candidate.ID)
}

func (r *Repo) ClaimJobByID(ctx context.Context, tenantID, id uint64, owner string, lease time.Duration) (*model.CompileJob, error) {
	now := time.Now().UTC()
	var candidate model.CompileJob
	query := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ? AND deleted_at IS NULL AND cancel_requested = ?", tenantID, id, false).
		Where("(status = ? OR (status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at < ?))", model.JobPending, model.JobRunning, now)
	if err := query.First(&candidate).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, biz.ErrNoPendingJob
		}
		return nil, fmt.Errorf("find wiki job %d: %w", id, err)
	}

	expires := now.Add(lease)
	claim := r.db.WithContext(ctx).Model(&model.CompileJob{}).Where("tenant_id = ? AND id = ? AND cancel_requested = ?", tenantID, id, false)
	if candidate.Status == model.JobPending {
		claim = claim.Where("status = ?", model.JobPending)
	} else {
		claim = claim.Where("status = ? AND lease_owner = ? AND lease_expires_at = ?", model.JobRunning, candidate.LeaseOwner, candidate.LeaseExpiresAt)
	}
	res := claim.Updates(map[string]any{"status": model.JobRunning, "stage": "claimed", "lease_owner": owner, "lease_expires_at": expires, "attempt": gorm.Expr("attempt + 1")})
	if res.Error != nil {
		return nil, fmt.Errorf("claim wiki job %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return nil, biz.ErrNoPendingJob
	}
	return r.GetJob(ctx, tenantID, id)
}

func (r *Repo) UpdateJob(ctx context.Context, tenantID, id uint64, status, stage, errMessage string) error {
	updates := map[string]any{"status": status, "stage": stage, "error_message": errMessage}
	if status != model.JobRunning {
		updates["lease_owner"] = ""
		updates["lease_expires_at"] = nil
		updates["active_key"] = nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&model.CompileJob{}).Where("tenant_id = ? AND id = ?", tenantID, id)
		if status != model.JobCancelled {
			query = query.Where("status <> ?", model.JobCancelled)
		}
		res := query.Updates(updates)
		if res.Error != nil {
			return fmt.Errorf("update wiki job: %w", res.Error)
		}
		if res.RowsAffected == 0 {
			return errs.ErrNotFound
		}
		if status != model.JobRunning {
			if err := unlockJobSources(tx, tenantID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repo) IsCancelRequested(ctx context.Context, tenantID, id uint64) (bool, error) {
	var row struct{ CancelRequested bool }
	if err := r.db.WithContext(ctx).Model(&model.CompileJob{}).Select("cancel_requested").Where("tenant_id = ? AND id = ?", tenantID, id).First(&row).Error; err != nil {
		return false, mapNotFound(err)
	}
	return row.CancelRequested, nil
}

func (r *Repo) ReplaceChunks(ctx context.Context, tenantID, versionID uint64, chunks []*model.SourceChunk) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("tenant_id = ? AND version_id = ?", tenantID, versionID).Delete(&model.SourceChunk{}).Error; err != nil {
			return fmt.Errorf("delete wiki chunks: %w", err)
		}
		if len(chunks) == 0 {
			return nil
		}
		if err := tx.Create(&chunks).Error; err != nil {
			return fmt.Errorf("create wiki chunks: %w", err)
		}
		return nil
	})
}

func (r *Repo) PublishPage(ctx context.Context, page *model.Page, sources []*model.PageSource) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return upsertPageTx(tx, page, sources)
	})
}

func upsertPageTx(tx *gorm.DB, page *model.Page, sources []*model.PageSource) error {
	pathHash := sha256.Sum256([]byte(page.RelativePath))
	pathHashHex := hex.EncodeToString(pathHash[:])
	page.RelativePathSHA256 = &pathHashHex
	var stored model.Page
	err := tx.Unscoped().Where("tenant_id = ? AND page_id = ?", page.TenantID, page.PageID).First(&stored).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		if err := tx.Create(page).Error; err != nil {
			return fmt.Errorf("create wiki page: %w", err)
		}
	case err != nil:
		return fmt.Errorf("find wiki page: %w", err)
	default:
		page.ID = stored.ID
		page.CreatedAt = stored.CreatedAt
		if err := tx.Model(&stored).Updates(map[string]any{
			"tenant_id":            page.TenantID,
			"page_id":              page.PageID,
			"page_type":            page.PageType,
			"title":                page.Title,
			"aliases_json":         page.AliasesJSON,
			"language":             page.Language,
			"relative_path":        page.RelativePath,
			"relative_path_sha256": page.RelativePathSHA256,
			"body_sha256":          page.BodySHA256,
			"deleted_at":           nil,
		}).Error; err != nil {
			return fmt.Errorf("update wiki page: %w", err)
		}
	}
	if err := tx.Unscoped().Where("tenant_id = ? AND page_id = ?", page.TenantID, page.PageID).Delete(&model.PageSource{}).Error; err != nil {
		return fmt.Errorf("replace wiki page sources: %w", err)
	}
	if len(sources) > 0 {
		if err := tx.Create(&sources).Error; err != nil {
			return fmt.Errorf("create wiki page sources: %w", err)
		}
	}
	return nil
}

func (r *Repo) ListPageSources(ctx context.Context, tenantID uint64, pageID string) ([]*model.PageSource, error) {
	var rows []*model.PageSource
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND page_id = ? AND deleted_at IS NULL", tenantID, pageID).
		Order("source_version_id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list wiki page sources: %w", err)
	}
	return rows, nil
}

func (r *Repo) ListPagesBySource(ctx context.Context, tenantID, sourceID uint64) ([]*model.Page, error) {
	var rows []*model.Page
	err := r.db.WithContext(ctx).
		Table("wiki_pages AS pages").
		Select("pages.*").
		Joins("JOIN wiki_page_sources AS page_sources ON page_sources.tenant_id = pages.tenant_id AND page_sources.page_id = pages.page_id AND page_sources.deleted_at IS NULL").
		Joins("JOIN source_versions AS versions ON versions.tenant_id = page_sources.tenant_id AND versions.id = page_sources.source_version_id AND versions.deleted_at IS NULL").
		Where("pages.tenant_id = ? AND pages.deleted_at IS NULL AND versions.source_id = ?", tenantID, sourceID).
		Order("pages.title ASC").
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("list wiki pages by source: %w", err)
	}
	return rows, nil
}

func (r *Repo) ReplaceRelations(ctx context.Context, tenantID uint64, fromPageID string, relations []*model.PageRelation) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Unscoped().Where("tenant_id = ? AND from_page_id = ?", tenantID, fromPageID).Delete(&model.PageRelation{}).Error; err != nil {
			return fmt.Errorf("replace wiki relations: %w", err)
		}
		if len(relations) == 0 {
			return nil
		}
		if err := tx.Create(&relations).Error; err != nil {
			return fmt.Errorf("create wiki relations: %w", err)
		}
		return nil
	})
}

func (r *Repo) ListPages(ctx context.Context, tenantID uint64) ([]*model.Page, error) {
	var rows []*model.Page
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL", tenantID).Order("title ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list wiki pages: %w", err)
	}
	return rows, nil
}

func (r *Repo) ListTopics(ctx context.Context, tenantID uint64) ([]*model.Topic, error) {
	var rows []*model.Topic
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND deleted_at IS NULL", tenantID).Order("title ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list wiki topics: %w", err)
	}
	return rows, nil
}

func (r *Repo) GetTopic(ctx context.Context, tenantID uint64, topicID string) (*model.Topic, error) {
	var row model.Topic
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND topic_id = ? AND deleted_at IS NULL", tenantID, topicID).First(&row).Error; err != nil {
		return nil, mapNotFound(err)
	}
	return &row, nil
}

func (r *Repo) ListTopicEvidence(ctx context.Context, tenantID uint64, topicID string) ([]*model.TopicEvidence, error) {
	var rows []*model.TopicEvidence
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND topic_id = ? AND deleted_at IS NULL", tenantID, topicID).
		Order("source_id ASC, source_version_id ASC, chunk_ordinal ASC, evidence_start ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list wiki topic evidence: %w", err)
	}
	return rows, nil
}

func (r *Repo) ListTopicsBySource(ctx context.Context, tenantID, sourceID uint64) ([]*model.Topic, error) {
	var rows []*model.Topic
	if err := r.db.WithContext(ctx).
		Table("topics AS topics").Select("topics.*").Distinct().
		Joins("JOIN topic_evidence AS evidence ON evidence.tenant_id = topics.tenant_id AND evidence.topic_id = topics.topic_id AND evidence.deleted_at IS NULL").
		Where("topics.tenant_id = ? AND topics.deleted_at IS NULL AND evidence.source_id = ?", tenantID, sourceID).
		Order("topics.title ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list wiki topics by source: %w", err)
	}
	return rows, nil
}

func (r *Repo) CommitTopicCompilation(ctx context.Context, tenantID, sourceID, versionID uint64, sourcePageID string, topics []*model.Topic, evidence []*model.TopicEvidence, batch biz.ArtifactBatch) error {
	pages := make([]*model.Page, 0, len(batch.Pages))
	pageSources := make(map[string][]*model.PageSource, len(batch.Pages))
	for _, page := range batch.Pages {
		aliases, err := json.Marshal(page.Aliases)
		if err != nil {
			return fmt.Errorf("encode artifact page aliases %q: %w", page.ID, err)
		}
		pages = append(pages, &model.Page{
			PageID: page.ID, TenantID: tenantID, PageType: page.Type, Title: page.Title,
			AliasesJSON: string(aliases), Language: page.Language, RelativePath: page.RelativePath,
			BodySHA256: page.BodySHA256, CreatedAt: page.CreatedAt, UpdatedAt: page.UpdatedAt,
		})
		for _, sourceVersionID := range batch.PageSourceVersionIDs[page.ID] {
			pageSources[page.ID] = append(pageSources[page.ID], &model.PageSource{TenantID: tenantID, PageID: page.ID, SourceVersionID: sourceVersionID})
		}
	}
	relations := make([]*model.PageRelation, 0, len(batch.Links))
	for _, link := range batch.Links {
		relations = append(relations, &model.PageRelation{TenantID: tenantID, FromPageID: link.FromPageID, ToPageID: link.ToPageID, RelationType: link.Type})
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		err = r.commitTopicCompilationOnce(ctx, tenantID, sourceID, versionID, sourcePageID, topics, evidence, pages, pageSources, relations, batch.DeletedPageIDs)
		if err == nil || !isDuplicateKey(err) {
			return err
		}
	}
	return err
}

func (r *Repo) commitTopicCompilationOnce(ctx context.Context, tenantID, sourceID, versionID uint64, sourcePageID string, topics []*model.Topic, evidence []*model.TopicEvidence, pages []*model.Page, pageSources map[string][]*model.PageSource, relations []*model.PageRelation, deletedPageIDs []string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, topic := range topics {
			if topic == nil {
				continue
			}
			var stored model.Topic
			err := tx.Unscoped().Where("tenant_id = ? AND topic_id = ?", tenantID, topic.TopicID).First(&stored).Error
			switch {
			case errors.Is(err, gorm.ErrRecordNotFound):
				topic.TenantID = tenantID
				if err := tx.Create(topic).Error; err != nil {
					return fmt.Errorf("create wiki topic %q: %w", topic.TopicID, err)
				}
			case err != nil:
				return fmt.Errorf("find wiki topic %q: %w", topic.TopicID, err)
			default:
				if err := tx.Model(&stored).Updates(map[string]any{
					"canonical_key": topic.CanonicalKey, "title": topic.Title, "aliases_json": topic.AliasesJSON,
					"entity_names_json": topic.EntityNamesJSON, "concept_names_json": topic.ConceptNamesJSON,
					"summary": topic.Summary, "generation_fingerprint": topic.GenerationFingerprint,
					"status": topic.Status, "deleted_at": nil,
				}).Error; err != nil {
					return fmt.Errorf("update wiki topic %q: %w", topic.TopicID, err)
				}
			}
		}
		if err := tx.Unscoped().Where("tenant_id = ? AND source_id = ?", tenantID, sourceID).Delete(&model.TopicEvidence{}).Error; err != nil {
			return fmt.Errorf("replace source topic evidence: %w", err)
		}
		if len(evidence) > 0 {
			if err := tx.Create(&evidence).Error; err != nil {
				return fmt.Errorf("create source topic evidence: %w", err)
			}
		}
		for _, topic := range topics {
			if topic == nil || topic.Status != model.TopicPending {
				continue
			}
			var count int64
			if err := tx.Model(&model.TopicEvidence{}).Where("tenant_id = ? AND topic_id = ? AND deleted_at IS NULL", tenantID, topic.TopicID).Count(&count).Error; err != nil {
				return fmt.Errorf("count pending wiki topic evidence: %w", err)
			}
			if count == 0 {
				if err := tx.Unscoped().Where("tenant_id = ? AND topic_id = ?", tenantID, topic.TopicID).Delete(&model.Topic{}).Error; err != nil {
					return fmt.Errorf("delete orphan wiki topic: %w", err)
				}
			}
		}
		for _, page := range pages {
			if page == nil {
				continue
			}
			if err := upsertPageTx(tx, page, pageSources[page.PageID]); err != nil {
				return err
			}
		}
		for _, pageID := range deletedPageIDs {
			if err := tx.Where("tenant_id = ? AND (from_page_id = ? OR to_page_id = ?)", tenantID, pageID, pageID).Delete(&model.PageRelation{}).Error; err != nil {
				return fmt.Errorf("delete stale wiki relations: %w", err)
			}
			if err := tx.Where("tenant_id = ? AND page_id = ?", tenantID, pageID).Delete(&model.PageSource{}).Error; err != nil {
				return fmt.Errorf("delete stale wiki page sources: %w", err)
			}
			if err := tx.Where("tenant_id = ? AND page_id = ?", tenantID, pageID).Delete(&model.Page{}).Error; err != nil {
				return fmt.Errorf("delete stale wiki page: %w", err)
			}
			if err := tx.Model(&model.Topic{}).Where("tenant_id = ? AND topic_id = ?", tenantID, pageID).Update("status", model.TopicPending).Error; err != nil {
				return fmt.Errorf("mark stale wiki topic: %w", err)
			}
		}
		if err := tx.Unscoped().Where("tenant_id = ? AND from_page_id = ?", tenantID, sourcePageID).Delete(&model.PageRelation{}).Error; err != nil {
			return fmt.Errorf("replace source wiki relations: %w", err)
		}
		if len(relations) > 0 {
			if err := tx.Create(&relations).Error; err != nil {
				return fmt.Errorf("create source wiki relations: %w", err)
			}
		}
		if err := tx.Model(&model.Source{}).Where("tenant_id = ? AND id = ?", tenantID, sourceID).Update("status", model.SourceSucceeded).Error; err != nil {
			return fmt.Errorf("mark compiled wiki source: %w", err)
		}
		if err := tx.Model(&model.SourceVersion{}).Where("tenant_id = ? AND id = ?", tenantID, versionID).Update("schema_version", biz.SchemaVersion).Error; err != nil {
			return fmt.Errorf("mark compiled wiki version: %w", err)
		}
		return nil
	})
}

func (r *Repo) MarkSourceStatus(ctx context.Context, tenantID, id uint64, status string) error {
	res := r.db.WithContext(ctx).Model(&model.Source{}).Where("tenant_id = ? AND id = ?", tenantID, id).Update("status", status)
	if res.Error != nil {
		return fmt.Errorf("mark wiki source: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return errs.ErrNotFound
	}
	return nil
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errs.ErrNotFound
	}
	return err
}

func isDuplicateKey(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate entry") || strings.Contains(message, "unique constraint failed")
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok || value == "" {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func lockJobSources(tx *gorm.DB, job *model.CompileJob) error {
	ids, err := biz.DecodeSourceIDs(job.SourceIDsJSON)
	if err != nil {
		return fmt.Errorf("decode wiki job sources: %w", err)
	}
	for _, sourceID := range ids {
		res := tx.Model(&model.Source{}).Where("tenant_id = ? AND id = ? AND active_job_id IS NULL AND deleted_at IS NULL", job.TenantID, sourceID).Update("active_job_id", job.ID)
		if res.Error != nil {
			return fmt.Errorf("lock wiki source %d: %w", sourceID, res.Error)
		}
		if res.RowsAffected == 0 {
			return errors.Join(errs.ErrConflict, fmt.Errorf("wiki source %d already has an active compile job", sourceID))
		}
	}
	return nil
}

func unlockJobSources(tx *gorm.DB, tenantID, jobID uint64) error {
	if err := tx.Model(&model.Source{}).Where("tenant_id = ? AND active_job_id = ?", tenantID, jobID).Update("active_job_id", nil).Error; err != nil {
		return fmt.Errorf("unlock wiki job sources: %w", err)
	}
	return nil
}
