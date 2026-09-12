// Package index implements the rebuildable Wiki lexical and vector indexes.
package index

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/embedding"
	"gorm.io/gorm"
)

const indexSchemaVersion = "1"

type Index struct {
	root  string
	db    *gorm.DB
	embed embedding.Embedder
	dim   int
}

type wikiVector struct {
	TenantID  uint64 `gorm:"column:tenant_id;primaryKey"`
	PageID    string `gorm:"column:page_id;size:64;primaryKey"`
	BodyHash  string `gorm:"column:body_hash;size:64;not null"`
	Dimension int    `gorm:"column:dimension;not null"`
	Vector    []byte `gorm:"column:vector;not null"`
}

func (wikiVector) TableName() string { return "wiki_vectors" }

type wikiIndexMeta struct {
	Key   string `gorm:"column:key;size:64;primaryKey"`
	Value string `gorm:"column:value;size:255;not null"`
}

func (wikiIndexMeta) TableName() string { return "wiki_index_meta" }

type wikiPageMetadata struct {
	PageID           string `gorm:"column:page_id"`
	PageType         string `gorm:"column:page_type"`
	Title            string `gorm:"column:title"`
	RelativePath     string `gorm:"column:relative_path"`
	AliasesJSON      string `gorm:"column:aliases_json"`
	EntityNamesJSON  string `gorm:"column:entity_names_json"`
	ConceptNamesJSON string `gorm:"column:concept_names_json"`
}

// New initializes search structures in the same SQLite database as Wiki state.
func New(ctx context.Context, root string, db *gorm.DB, embed embedding.Embedder, dim int) (*Index, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("llmwiki index: file root is required")
	}
	if db == nil {
		return nil, errors.New("llmwiki index: database is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dim <= 0 && embed != nil {
		dim = embed.Dim()
	}
	if err := db.WithContext(ctx).AutoMigrate(&wikiVector{}, &wikiIndexMeta{}); err != nil {
		return nil, fmt.Errorf("llmwiki index: migrate tables: %w", err)
	}
	const createFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS wiki_fts USING fts5(
		page_key UNINDEXED, tenant_id UNINDEXED, page_id UNINDEXED, page_type UNINDEXED,
		title, aliases, content, tokenize='trigram'
	)`
	if err := db.WithContext(ctx).Exec(createFTS).Error; err != nil {
		return nil, fmt.Errorf("llmwiki index: create FTS5 trigram index: %w", err)
	}
	meta := wikiIndexMeta{Key: "schema_version", Value: indexSchemaVersion}
	if err := db.WithContext(ctx).Save(&meta).Error; err != nil {
		return nil, fmt.Errorf("llmwiki index: save schema version: %w", err)
	}
	return &Index{root: root, db: db, embed: embed, dim: dim}, nil
}

func (i *Index) IndexPage(ctx context.Context, document biz.IndexDocument) error {
	if document.Page == nil {
		return errors.New("llmwiki index: page is required")
	}
	page := document.Page
	pageKey := fmt.Sprintf("%d:%s", page.TenantID, page.PageID)
	aliases := strings.Join(document.Aliases, "\n")
	if aliases == "" {
		aliases = strings.Join(decodeJSONStrings(page.AliasesJSON), "\n")
	}
	if err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM wiki_fts WHERE page_key = ?", pageKey).Error; err != nil {
			return fmt.Errorf("delete old FTS page: %w", err)
		}
		if err := tx.Exec(
			"INSERT INTO wiki_fts (page_key, tenant_id, page_id, page_type, title, aliases, content) VALUES (?, ?, ?, ?, ?, ?, ?)",
			pageKey, strconv.FormatUint(page.TenantID, 10), page.PageID, page.PageType, page.Title, aliases, document.Content,
		).Error; err != nil {
			return fmt.Errorf("insert FTS page: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("llmwiki index: update lexical page: %w", err)
	}

	if i.embed == nil {
		return nil
	}
	bodyHash := contentHash(document.Content)
	var existing wikiVector
	err := i.db.WithContext(ctx).Where("tenant_id = ? AND page_id = ?", page.TenantID, page.PageID).First(&existing).Error
	if err == nil && existing.BodyHash == bodyHash && (i.dim <= 0 || existing.Dimension == i.dim) {
		return nil
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("llmwiki index: load existing vector: %w", err)
	}
	vectors, err := i.embed.Embed(ctx, []string{page.Title + "\n\n" + document.Content})
	if err != nil {
		if deleteErr := i.deleteVector(ctx, page.TenantID, page.PageID); deleteErr != nil {
			return errors.Join(fmt.Errorf("llmwiki index: embed page: %w", err), deleteErr)
		}
		return fmt.Errorf("llmwiki index: embed page: %w", err)
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		cause := errors.New("llmwiki index: embedder returned an unexpected vector count")
		if deleteErr := i.deleteVector(ctx, page.TenantID, page.PageID); deleteErr != nil {
			return errors.Join(cause, deleteErr)
		}
		return cause
	}
	if i.dim > 0 && len(vectors[0]) != i.dim {
		cause := fmt.Errorf("llmwiki index: vector dimension %d does not match %d", len(vectors[0]), i.dim)
		if deleteErr := i.deleteVector(ctx, page.TenantID, page.PageID); deleteErr != nil {
			return errors.Join(cause, deleteErr)
		}
		return cause
	}
	row := wikiVector{TenantID: page.TenantID, PageID: page.PageID, BodyHash: bodyHash, Dimension: len(vectors[0]), Vector: encodeVector(vectors[0])}
	if err := i.db.WithContext(ctx).Save(&row).Error; err != nil {
		return fmt.Errorf("llmwiki index: save vector: %w", err)
	}
	return nil
}

func (i *Index) DeletePage(ctx context.Context, page *model.Page) error {
	if page == nil {
		return errors.New("llmwiki index: page is required")
	}
	pageKey := fmt.Sprintf("%d:%s", page.TenantID, page.PageID)
	return i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM wiki_fts WHERE page_key = ?", pageKey).Error; err != nil {
			return fmt.Errorf("llmwiki index: delete FTS page: %w", err)
		}
		if err := tx.Where("tenant_id = ? AND page_id = ?", page.TenantID, page.PageID).Delete(&wikiVector{}).Error; err != nil {
			return fmt.Errorf("llmwiki index: delete vector page: %w", err)
		}
		return nil
	})
}

// Rebuild replaces only Markdown-derived wiki_* rows. Durable source, job,
// topic and evidence state in the same database is intentionally untouched.
func (i *Index) Rebuild(ctx context.Context, tenantID uint64, files *biz.FileStore) ([]string, error) {
	if files == nil {
		return nil, errors.New("llmwiki index: file store is required for rebuild")
	}
	pages, links, warnings, err := files.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	validPageIDs := make([]string, 0, len(pages))
	models := make([]*model.Page, 0, len(pages))
	pageSources := make([]*model.PageSource, 0)
	for _, page := range pages {
		aliases, marshalErr := json.Marshal(page.Aliases)
		if marshalErr != nil {
			return nil, fmt.Errorf("llmwiki index: encode aliases for %s: %w", page.ID, marshalErr)
		}
		pathHash := contentHash(page.RelativePath)
		models = append(models, &model.Page{
			PageID: page.ID, TenantID: tenantID, PageType: page.Type, Title: page.Title,
			AliasesJSON: string(aliases), Language: page.Language, RelativePath: page.RelativePath,
			RelativePathSHA256: &pathHash, BodySHA256: page.BodySHA256,
		})
		validPageIDs = append(validPageIDs, page.ID)
		for _, versionID := range page.SourceVersions {
			pageSources = append(pageSources, &model.PageSource{TenantID: tenantID, PageID: page.ID, SourceVersionID: versionID})
		}
	}
	relations := make([]*model.PageRelation, 0, len(links))
	for _, link := range links {
		relations = append(relations, &model.PageRelation{TenantID: tenantID, FromPageID: link.FromPageID, ToPageID: link.ToPageID, RelationType: link.Type})
	}
	if err := i.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&wikiIndexMeta{Key: "build_status", Value: "building"}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM wiki_fts WHERE tenant_id = ?", strconv.FormatUint(tenantID, 10)).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Where("tenant_id = ?", tenantID).Delete(&model.PageRelation{}).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Where("tenant_id = ?", tenantID).Delete(&model.PageSource{}).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Where("tenant_id = ?", tenantID).Delete(&model.Page{}).Error; err != nil {
			return err
		}
		if len(models) > 0 {
			if err := tx.Create(&models).Error; err != nil {
				return err
			}
		}
		if len(pageSources) > 0 {
			if err := tx.Create(&pageSources).Error; err != nil {
				return err
			}
		}
		if len(relations) > 0 {
			if err := tx.Create(&relations).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("llmwiki index: replace derived catalog: %w", err)
	}
	for index, page := range pages {
		body, readErr := files.Read(ctx, filepath.ToSlash(filepath.Join("wiki", page.RelativePath)), biz.MaxPageBytes)
		if readErr != nil {
			return nil, readErr
		}
		if err := i.IndexPage(ctx, biz.IndexDocument{Page: models[index], Content: string(body), SourceVersionIDs: page.SourceVersions, Aliases: page.Aliases}); err != nil {
			warnings = append(warnings, fmt.Sprintf("page=%s index=%v", page.ID, err))
		}
	}
	vectorDelete := i.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if len(validPageIDs) > 0 {
		vectorDelete = vectorDelete.Where("page_id NOT IN ?", validPageIDs)
	}
	if err := vectorDelete.Delete(&wikiVector{}).Error; err != nil {
		return nil, fmt.Errorf("llmwiki index: remove orphan vectors: %w", err)
	}
	if err := i.db.WithContext(ctx).Save(&wikiIndexMeta{Key: "build_status", Value: "ready"}).Error; err != nil {
		return nil, fmt.Errorf("llmwiki index: mark rebuild ready: %w", err)
	}
	return warnings, nil
}

func (i *Index) deleteVector(ctx context.Context, tenantID uint64, pageID string) error {
	if err := i.db.WithContext(ctx).Where("tenant_id = ? AND page_id = ?", tenantID, pageID).Delete(&wikiVector{}).Error; err != nil {
		return fmt.Errorf("llmwiki index: delete stale vector: %w", err)
	}
	return nil
}

func (i *Index) Search(ctx context.Context, tenantID uint64, query string, limit int) ([]biz.SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 10
	}
	lexical, lexicalErr := i.searchLexical(ctx, tenantID, query, limit*2)
	vector, vectorErr := i.searchVector(ctx, tenantID, query, limit*2)
	if lexicalErr != nil && vectorErr != nil {
		return nil, errors.Join(lexicalErr, vectorErr)
	}
	return mergeRRF(query, lexical, vector, limit), nil
}

func (i *Index) searchLexical(ctx context.Context, tenantID uint64, query string, limit int) ([]biz.SearchHit, error) {
	type lexicalRow struct {
		PageID   string  `gorm:"column:page_id"`
		PageType string  `gorm:"column:page_type"`
		Title    string  `gorm:"column:title"`
		Preview  string  `gorm:"column:preview"`
		Rank     float64 `gorm:"column:rank"`
	}
	var rows []lexicalRow
	tenant := strconv.FormatUint(tenantID, 10)
	var err error
	if len([]rune(query)) < 3 {
		pattern := "%" + query + "%"
		const fallback = `SELECT page_id, page_type, title, substr(content, 1, 800) AS preview, 0 AS rank
			FROM wiki_fts WHERE tenant_id = ? AND (title LIKE ? OR aliases LIKE ? OR content LIKE ?) LIMIT ?`
		err = i.db.WithContext(ctx).Raw(fallback, tenant, pattern, pattern, pattern, limit).Scan(&rows).Error
	} else {
		match := `"` + strings.ReplaceAll(strings.TrimSpace(query), `"`, `""`) + `"`
		const statement = `SELECT page_id, page_type, title,
			snippet(wiki_fts, 6, '', '', '…', 32) AS preview, bm25(wiki_fts) AS rank
			FROM wiki_fts WHERE wiki_fts MATCH ? AND tenant_id = ? ORDER BY rank LIMIT ?`
		err = i.db.WithContext(ctx).Raw(statement, match, tenant, limit).Scan(&rows).Error
	}
	if err != nil {
		return nil, fmt.Errorf("llmwiki index: search FTS: %w", err)
	}
	hits := make([]biz.SearchHit, 0, len(rows))
	for _, row := range rows {
		score := 1 / (1 + math.Abs(row.Rank))
		hits = append(hits, biz.SearchHit{Layer: "wiki", PageID: row.PageID, PageType: row.PageType, Title: row.Title, Preview: row.Preview, Score: score})
	}
	return hits, nil
}

func (i *Index) searchVector(ctx context.Context, tenantID uint64, query string, limit int) ([]biz.SearchHit, error) {
	if i.embed == nil {
		return nil, nil
	}
	vectors, err := i.embed.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("llmwiki index: embed query: %w", err)
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return nil, errors.New("llmwiki index: embedder returned an unexpected query vector count")
	}
	var rows []wikiVector
	if err := i.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("llmwiki index: load vectors: %w", err)
	}
	hits := make([]biz.SearchHit, 0, len(rows))
	for _, row := range rows {
		vector, decodeErr := decodeVector(row.Vector, row.Dimension)
		if decodeErr != nil || len(vector) != len(vectors[0]) {
			continue
		}
		var page wikiPageMetadata
		err := i.db.WithContext(ctx).Table("wiki_pages AS pages").
			Select("pages.page_id, pages.page_type, pages.title, pages.relative_path, pages.aliases_json, topics.entity_names_json, topics.concept_names_json").
			Joins("LEFT JOIN topics AS topics ON topics.tenant_id = pages.tenant_id AND topics.topic_id = pages.page_id AND topics.deleted_at IS NULL").
			Where("pages.tenant_id = ? AND pages.page_id = ? AND pages.deleted_at IS NULL", tenantID, row.PageID).First(&page).Error
		if err != nil {
			continue
		}
		body, readErr := os.ReadFile(filepath.Join(i.root, "wiki", filepath.FromSlash(page.RelativePath)))
		if readErr != nil {
			continue
		}
		preview := string(body)
		if len([]rune(preview)) > 800 {
			preview = string([]rune(preview)[:800]) + "…"
		}
		nodes := append(decodeJSONStrings(page.EntityNamesJSON), decodeJSONStrings(page.ConceptNamesJSON)...)
		hits = append(hits, biz.SearchHit{Layer: "wiki", PageID: page.PageID, PageType: page.PageType, Title: page.Title, Preview: preview, Score: cosine(vectors[0], vector), MatchedNode: matchedNode(query, nodes)})
	}
	sort.Slice(hits, func(left, right int) bool { return hits[left].Score > hits[right].Score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

func mergeRRF(query string, lists []biz.SearchHit, vector []biz.SearchHit, limit int) []biz.SearchHit {
	byID := make(map[string]biz.SearchHit)
	for _, list := range [][]biz.SearchHit{lists, vector} {
		for rank, hit := range list {
			existing := byID[hit.PageID]
			if existing.PageID == "" {
				existing = hit
				existing.Score = 0
			}
			existing.Score += 1 / float64(60+rank+1)
			byID[hit.PageID] = existing
		}
	}
	out := make([]biz.SearchHit, 0, len(byID))
	for _, hit := range byID {
		if strings.Contains(strings.ToLower(hit.Title), strings.ToLower(strings.TrimSpace(query))) {
			hit.Score += 0.005
		}
		out = append(out, hit)
	}
	sort.Slice(out, func(left, right int) bool {
		if out[left].Score == out[right].Score {
			return out[left].PageID < out[right].PageID
		}
		return out[left].Score > out[right].Score
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func encodeVector(vector []float32) []byte {
	body := make([]byte, len(vector)*4)
	for index, value := range vector {
		binary.LittleEndian.PutUint32(body[index*4:], math.Float32bits(value))
	}
	return body
}

func decodeVector(body []byte, dimension int) ([]float32, error) {
	if dimension <= 0 || len(body) != dimension*4 {
		return nil, errors.New("invalid float32 vector blob")
	}
	vector := make([]float32, dimension)
	for index := range vector {
		vector[index] = math.Float32frombits(binary.LittleEndian.Uint32(body[index*4:]))
	}
	return vector, nil
}

func cosine(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		l := float64(left[index])
		r := float64(right[index])
		dot += l * r
		leftNorm += l * l
		rightNorm += r * r
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / (math.Sqrt(leftNorm) * math.Sqrt(rightNorm))
}

func decodeJSONStrings(raw string) []string {
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	return values
}

func matchedNode(query string, nodes []string) string {
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	for _, node := range nodes {
		value := strings.ToLower(strings.TrimSpace(node))
		if value == "" {
			continue
		}
		for _, term := range terms {
			if strings.Contains(value, term) || strings.Contains(term, value) {
				return node
			}
		}
	}
	return ""
}
