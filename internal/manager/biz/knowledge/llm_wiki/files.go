package llm_wiki

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
)

//go:embed schema_v1.md
var builtinSchema string

func BuiltinSchema() string { return builtinSchema }

type FileStore struct {
	root       string
	artifactMu sync.Mutex
}

var errChunkCacheInvalid = errors.New("llmwiki: invalid chunk cache")

type MirroredFile struct {
	RawPath      string
	SnapshotPath string
	SHA256       string
	Size         uint64
}

func NewFileStore(root string) (*FileStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, errors.New("llmwiki: root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("llmwiki: resolve root: %w", err)
	}
	return &FileStore{root: abs}, nil
}

func (s *FileStore) Root() string { return s.root }

func (s *FileStore) LockArtifacts() func() {
	s.artifactMu.Lock()
	return s.artifactMu.Unlock
}

func (s *FileStore) Ensure(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, dir := range []string{"raw", "wiki/sources", "wiki/topics", ".llm-wiki/versions", ".llm-wiki/chunks", ".llm-wiki/summaries", ".llm-wiki/chunk-cache", ".llm-wiki/staging"} {
		path, err := s.resolve("", dir)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o750); err != nil {
			return fmt.Errorf("llmwiki: create %s: %w", dir, err)
		}
	}
	schemaPath, err := s.resolve("", "schema.md")
	if err != nil {
		return err
	}
	if err := atomicWrite(schemaPath, []byte(BuiltinSchema()), 0o640); err != nil {
		return err
	}
	for relative, body := range map[string]string{"wiki/index.md": initialWikiIndex, "wiki/log.md": initialWikiLog} {
		path, err := s.resolve("", relative)
		if err != nil {
			return err
		}
		if err := ensureFile(path, []byte(body), 0o640); err != nil {
			return fmt.Errorf("llmwiki: initialize %s: %w", relative, err)
		}
	}
	return nil
}

var safeNameRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safeName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	name = safeNameRE.ReplaceAllString(name, "-")
	name = strings.Trim(name, ".-")
	if name == "" {
		name = "source.md"
	}
	if filepath.Ext(name) == "" {
		name += ".md"
	}
	return name
}

func sourceRelative(tenantID uint64, sourceType, sourceKey, name string) string {
	if sourceType == "upload" {
		fileName := safeUploadName(name)
		if tenantID == DefaultTenantID {
			return fileName
		}
		return filepath.ToSlash(filepath.Join(fmt.Sprintf("tenant-%d", tenantID), fileName))
	}
	parts := make([]string, 0, 3)
	if tenantID != DefaultTenantID {
		parts = append(parts, fmt.Sprintf("tenant-%d", tenantID))
	}
	parts = append(parts, sourceNamespace(sourceType, sourceKey))
	switch sourceType {
	case "repo", "git":
		parts = append(parts, safeRelativeName(name))
	default:
		parts = append(parts, safeName(name))
	}
	return filepath.ToSlash(filepath.Join(parts...))
}

// safeUploadName keeps the user's uploaded filename, including Unicode,
// spaces, and its original extension. Only path separators and control
// characters are removed so the raw tree does not silently rename documents
// such as "设计方案.pdf" to an unrelated markdown filename.
func safeUploadName(name string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(name), func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return "source"
	}
	name = strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) {
			return -1
		}
		return r
	}, parts[len(parts)-1])
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "source"
	}
	return name
}

func sourceNamespace(sourceType, sourceKey string) string {
	parts := strings.Split(sourceKey, ":")
	identifier := sourceKey
	if len(parts) > 1 {
		identifier = parts[1]
	}
	if identifier == "" {
		identifier = "source"
	}
	name := safeDirName(sourceType + "-" + identifier)
	if name == "" {
		return "source"
	}
	return name
}

func safeDirName(name string) string {
	name = safeNameRE.ReplaceAllString(strings.TrimSpace(name), "-")
	return strings.Trim(name, ".-")
}

func safeRelativeName(name string) string {
	parts := strings.FieldsFunc(filepath.ToSlash(name), func(r rune) bool { return r == '/' || r == '\\' })
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "." && part != ".." {
			part = safeNameRE.ReplaceAllString(strings.Trim(part, ".-"), "-")
			if part == "" {
				continue
			}
			clean = append(clean, part)
		}
	}
	if len(clean) == 0 {
		return "source.md"
	}
	if filepath.Ext(clean[len(clean)-1]) == "" {
		clean[len(clean)-1] += ".md"
	}
	return filepath.Join(clean...)
}

func (s *FileStore) Mirror(ctx context.Context, in MirrorInput) (*MirroredFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.SourceKey) == "" {
		return nil, errors.New("llmwiki: source key is required")
	}
	if len(in.Content) > MaxSourceBytes {
		return nil, fmt.Errorf("llmwiki: source exceeds %d bytes", MaxSourceBytes)
	}
	sum := sha256.Sum256(in.Content)
	digest := hex.EncodeToString(sum[:])
	rel := sourceRelative(in.TenantID, in.SourceType, in.SourceKey, in.Name)
	raw, err := s.resolve("raw", rel)
	if err != nil {
		return nil, err
	}
	sourceSum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", in.TenantID, in.SourceKey)))
	versionRel := filepath.ToSlash(filepath.Join(".llm-wiki/versions", hex.EncodeToString(sourceSum[:12]), digest+".md"))
	version, err := s.resolve("", versionRel)
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(version, in.Content, 0o640); err != nil {
		return nil, fmt.Errorf("llmwiki: write version: %w", err)
	}
	if err := atomicWrite(raw, in.Content, 0o640); err != nil {
		return nil, fmt.Errorf("llmwiki: write raw: %w", err)
	}
	return &MirroredFile{RawPath: rel, SnapshotPath: versionRel, SHA256: digest, Size: uint64(len(in.Content))}, nil
}

func (s *FileStore) Read(ctx context.Context, relative string, max int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.resolve("", relative)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("llmwiki: open %s: %w", relative, err)
	}
	defer func() { _ = f.Close() /* read-only cleanup; the read result remains authoritative */ }()
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("llmwiki: read %s: %w", relative, err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("llmwiki: file exceeds %d bytes", max)
	}
	return body, nil
}

func (s *FileStore) PublishPage(ctx context.Context, pageType, relative string, body []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(body) > MaxPageBytes {
		return "", fmt.Errorf("llmwiki: page exceeds %d bytes", MaxPageBytes)
	}
	layer, err := pageStorageLayer(pageType)
	if err != nil {
		return "", err
	}
	path, err := s.resolve(layer, relative)
	if err != nil {
		return "", err
	}
	if err := atomicWrite(path, body, 0o640); err != nil {
		return "", fmt.Errorf("llmwiki: publish page: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func (s *FileStore) StagePage(ctx context.Context, jobID uint64, pageType, relative string, body []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if len(body) > MaxPageBytes {
		return "", fmt.Errorf("llmwiki: page exceeds %d bytes", MaxPageBytes)
	}
	if _, err := pageStorageLayer(pageType); err != nil {
		return "", err
	}
	path, err := s.resolve("", filepath.ToSlash(filepath.Join(".llm-wiki/staging", fmt.Sprintf("%d", jobID), "wiki", relative)))
	if err != nil {
		return "", err
	}
	if err := atomicWrite(path, body, 0o640); err != nil {
		return "", fmt.Errorf("llmwiki: stage page: %w", err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func (s *FileStore) ReadStagedPage(ctx context.Context, jobID uint64, relative string) ([]byte, error) {
	path := filepath.ToSlash(filepath.Join(".llm-wiki/staging", fmt.Sprintf("%d", jobID), "wiki", relative))
	return s.Read(ctx, path, MaxPageBytes)
}

func (s *FileStore) ActivateManifest(ctx context.Context, manifest PublishManifest) error {
	for _, page := range manifest.Pages {
		if page == nil {
			continue
		}
		body, err := s.ReadStagedPage(ctx, manifest.JobID, page.RelativePath)
		if errors.Is(err, fs.ErrNotExist) {
			activePath, pathErr := pageStorageRelative(page)
			if pathErr != nil {
				return pathErr
			}
			body, err = s.Read(ctx, activePath, MaxPageBytes)
		}
		if err != nil {
			return fmt.Errorf("llmwiki: read staged page %s: %w", page.RelativePath, err)
		}
		if contentSHA256(body) != page.BodySHA256 {
			return fmt.Errorf("llmwiki: staged page hash mismatch: %s", page.RelativePath)
		}
		if _, err := s.PublishPage(ctx, page.PageType, page.RelativePath, body); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileStore) RemovePage(ctx context.Context, pageType, relative string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	layer, err := pageStorageLayer(pageType)
	if err != nil {
		return err
	}
	path, err := s.resolve(layer, relative)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("llmwiki: remove page: %w", err)
	}
	return nil
}

func (s *FileStore) RemoveRaw(ctx context.Context, relative string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolve("raw", relative)
	if err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("llmwiki: remove raw file: %w", err)
	}
	return nil
}

func pageStorageLayer(pageType string) (string, error) {
	switch pageType {
	case model.PageTypeSource:
		return "wiki", nil
	case model.PageTypeTopic:
		return "wiki", nil
	default:
		return "", fmt.Errorf("llmwiki: unsupported page type %q", pageType)
	}
}

func (s *FileStore) WriteChunkSummary(ctx context.Context, versionID uint64, ordinal int, body []byte) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	relative := filepath.ToSlash(filepath.Join(".llm-wiki/chunks", fmt.Sprintf("%d", versionID), fmt.Sprintf("%06d.json", ordinal)))
	path, err := s.resolve("", relative)
	if err != nil {
		return "", "", err
	}
	if err := atomicWrite(path, body, 0o640); err != nil {
		return "", "", fmt.Errorf("llmwiki: write chunk summary: %w", err)
	}
	sum := sha256.Sum256(body)
	return relative, hex.EncodeToString(sum[:]), nil
}

func chunkCacheRelativePath(key ChunkCacheKey) string {
	material := fmt.Sprintf("%d\x00%s\x00%s\x00%s\x00%s", key.TenantID, key.ContentHash, key.PromptVersion, key.ModelVersion, ChunkCacheSchemaVersion)
	sum := sha256.Sum256([]byte(material))
	digest := hex.EncodeToString(sum[:])
	return filepath.ToSlash(filepath.Join(".llm-wiki/chunk-cache", fmt.Sprintf("%d", key.TenantID), digest[:2], digest+".json"))
}

type ChunkCacheKey struct {
	TenantID      uint64
	ContentHash   string
	PromptVersion string
	ModelVersion  string
}

func (s *FileStore) ReadChunkCache(ctx context.Context, key ChunkCacheKey) (*ChunkAnalysisCache, error) {
	body, err := s.Read(ctx, chunkCacheRelativePath(key), MaxPageBytes)
	if err != nil {
		return nil, err
	}
	var cache ChunkAnalysisCache
	if err := json.Unmarshal(body, &cache); err != nil {
		return nil, fmt.Errorf("%w: decode chunk cache: %w", errChunkCacheInvalid, err)
	}
	if cache.CacheSchema != ChunkCacheSchemaVersion || cache.ContentHash != key.ContentHash || cache.PromptVersion != key.PromptVersion || cache.ModelVersion != key.ModelVersion {
		return nil, fmt.Errorf("%w: key mismatch", errChunkCacheInvalid)
	}
	return &cache, nil
}

func (s *FileStore) WriteChunkCache(ctx context.Context, key ChunkCacheKey, summary ChunkSummaryIR) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(ChunkAnalysisCache{
		CacheSchema: ChunkCacheSchemaVersion, ContentHash: key.ContentHash,
		PromptVersion: key.PromptVersion, ModelVersion: key.ModelVersion, Summary: summary,
	})
	if err != nil {
		return fmt.Errorf("llmwiki: encode chunk cache: %w", err)
	}
	path, err := s.resolve("", chunkCacheRelativePath(key))
	if err != nil {
		return err
	}
	if err := atomicWrite(path, body, 0o640); err != nil {
		return fmt.Errorf("llmwiki: write chunk cache: %w", err)
	}
	return nil
}

func (s *FileStore) WriteSourceSummary(ctx context.Context, versionID uint64, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolve("", sourceSummaryPath(versionID))
	if err != nil {
		return err
	}
	if err := atomicWrite(path, body, 0o640); err != nil {
		return fmt.Errorf("llmwiki: write source summary: %w", err)
	}
	return nil
}

func (s *FileStore) ReadSourceSummary(ctx context.Context, versionID uint64) (*ChunkSummaryIR, error) {
	body, err := s.Read(ctx, sourceSummaryPath(versionID), MaxPageBytes)
	if err != nil {
		return nil, err
	}
	var summary ChunkSummaryIR
	if err := json.Unmarshal(body, &summary); err != nil {
		return nil, fmt.Errorf("llmwiki: decode source summary %d: %w", versionID, err)
	}
	return &summary, nil
}

func sourceSummaryPath(versionID uint64) string {
	return filepath.ToSlash(filepath.Join(".llm-wiki/summaries", fmt.Sprintf("%d.json", versionID)))
}

func (s *FileStore) StageManifest(ctx context.Context, manifest PublishManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("llmwiki: encode staging manifest: %w", err)
	}
	path, err := s.resolve("", filepath.ToSlash(filepath.Join(".llm-wiki/staging", fmt.Sprintf("%d", manifest.JobID), "manifest.json")))
	if err != nil {
		return err
	}
	if err := atomicWrite(path, body, 0o640); err != nil {
		return fmt.Errorf("llmwiki: write staging manifest: %w", err)
	}
	return nil
}

func (s *FileStore) ListManifests(ctx context.Context) ([]PublishManifest, error) {
	root, err := s.resolve("", ".llm-wiki/staging")
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("llmwiki: list staging manifests: %w", err)
	}
	manifests := make([]PublishManifest, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		relative := filepath.ToSlash(filepath.Join(".llm-wiki/staging", entry.Name(), "manifest.json"))
		body, err := s.Read(ctx, relative, 4<<20)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var manifest PublishManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return nil, fmt.Errorf("llmwiki: decode staging manifest %s: %w", entry.Name(), err)
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
}

func (s *FileStore) FinishManifest(ctx context.Context, jobID uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.resolve("", filepath.ToSlash(filepath.Join(".llm-wiki/staging", fmt.Sprintf("%d", jobID))))
	if err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("llmwiki: remove staging manifest: %w", err)
	}
	return nil
}

func (s *FileStore) resolve(layer, relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("llmwiki: absolute path rejected")
	}
	clean := filepath.Clean(filepath.FromSlash(relative))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("llmwiki: path traversal rejected")
	}
	base := s.root
	if layer != "" {
		base = filepath.Join(base, layer)
	}
	target := filepath.Join(base, clean)
	if target != base && !strings.HasPrefix(target, base+string(filepath.Separator)) {
		return "", errors.New("llmwiki: path escapes root")
	}
	if info, err := os.Lstat(target); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("llmwiki: symlink target rejected")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("llmwiki: inspect target: %w", err)
	}
	parent := filepath.Dir(target)
	for current := parent; strings.HasPrefix(current, s.root); current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("llmwiki: symlink path rejected")
		}
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("llmwiki: inspect path: %w", err)
		}
		if current == s.root {
			break
		}
	}
	return target, nil
}

func atomicWrite(path string, body []byte, mode fs.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == string(body) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".llmwiki-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName) /* best-effort cleanup after the primary write error */
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close() /* cleanup after the primary chmod error */
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close() /* cleanup after the primary write error */
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close() /* cleanup after the primary sync error */
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	keep = true
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() /* Sync below reports the durability error */ }()
	return dir.Sync()
}

func ensureFile(path string, body []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close() // close cannot improve the primary write failure
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close() // close cannot improve the primary sync failure
		return err
	}
	return file.Close()
}
