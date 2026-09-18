package llm_wiki

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
)

// Mirror stores one source file and records the new version. changed reports
// whether the content differed from the stored version, so callers can skip a
// recompilation for an unchanged upload.
func (u *Usecase) Mirror(ctx context.Context, input MirrorInput) (*model.Source, bool, error) {
	mirrored, err := u.files.MirrorSource(ctx, input)
	if err != nil {
		return nil, false, err
	}
	source := &model.Source{
		TenantID:      input.TenantID,
		SourceKey:     input.SourceKey,
		SourceType:    input.SourceType,
		RawPath:       mirrored.RawPath,
		ContentSHA256: mirrored.SHA256,
		Status:        model.SourcePending,
	}
	version := &model.SourceVersion{
		TenantID:      input.TenantID,
		SHA256:        mirrored.SHA256,
		SizeBytes:     mirrored.Size,
		SnapshotPath:  mirrored.SnapshotPath,
		SchemaVersion: SchemaVersion,
	}
	stored, _, changed, err := u.repo.UpsertSourceVersion(ctx, source, version)
	if err != nil {
		return nil, false, fmt.Errorf("llmwiki: save mirror metadata: %w", err)
	}
	return stored, changed, nil
}

type OrganizationSource struct {
	ID                   uint64
	Title, Path, Content string
}
type SyncResult struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Deleted   int `json:"deleted"`
	Total     int `json:"total"`
}

// SyncOrganizationSources makes the Wiki raw tree an exact projection of the
// organization knowledge base while retaining its folder hierarchy.
func (u *Usecase) SyncOrganizationSources(ctx context.Context, docs []OrganizationSource) (*SyncResult, error) {
	unlock := u.files.LockArtifacts()
	defer unlock()
	existing, _, err := u.repo.ListSources(ctx, DefaultTenantID, "", 10000)
	if err != nil {
		return nil, fmt.Errorf("llmwiki: list sources for sync: %w", err)
	}
	byKey := make(map[string]*model.Source)
	for _, source := range existing {
		if source.SourceType == "organization" {
			byKey[source.SourceKey] = source
		}
	}
	result := &SyncResult{Total: len(docs)}
	seen := make(map[string]bool, len(docs))
	for _, doc := range docs {
		key := fmt.Sprintf("organization:%d", doc.ID)
		seen[key] = true
		name := strings.TrimSpace(doc.Title)
		if name == "" {
			name = fmt.Sprintf("document-%d", doc.ID)
		}
		relative := filepath.ToSlash(filepath.Join(doc.Path, name+".md"))
		old := byKey[key]
		stored, changed, mirrorErr := u.Mirror(ctx, MirrorInput{TenantID: DefaultTenantID, SourceKey: key, SourceType: "organization", Name: name, Content: []byte(doc.Content), RelativePath: relative})
		if mirrorErr != nil {
			return nil, mirrorErr
		}
		if old == nil {
			result.Created++
		} else if changed || old.RawPath != stored.RawPath {
			result.Updated++
		} else {
			result.Unchanged++
		}
		if old != nil && old.RawPath != stored.RawPath {
			if err := u.files.RemoveSourceFile(ctx, old.RawPath); err != nil {
				return nil, err
			}
		}
	}
	for key, source := range byKey {
		if !seen[key] {
			if err := u.deleteSource(ctx, source.RawPath); err != nil {
				return nil, err
			}
			result.Deleted++
		}
	}
	return result, nil
}

// ListSources lists the mirrored sources of a tenant, optionally filtered by
// status.
func (u *Usecase) ListSources(ctx context.Context, tenantID uint64, status string, limit int) ([]*model.Source, int64, error) {
	return u.repo.ListSources(ctx, tenantID, status, limit)
}
