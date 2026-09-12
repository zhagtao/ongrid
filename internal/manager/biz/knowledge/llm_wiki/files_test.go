package llm_wiki

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
)

func TestFileStoreMirror_WhenContentChanges_PreservesVersions(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := store.Mirror(context.Background(), MirrorInput{SourceKey: "upload:guide.md", SourceType: "upload", Name: "guide.md", Content: []byte("first")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Mirror(context.Background(), MirrorInput{SourceKey: "upload:guide.md", SourceType: "upload", Name: "guide.md", Content: []byte("second")})
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotPath == second.SnapshotPath {
		t.Fatal("changed content reused immutable snapshot")
	}
	for _, relative := range []string{first.SnapshotPath, second.SnapshotPath} {
		if _, err := os.Stat(filepath.Join(store.Root(), relative)); err != nil {
			t.Fatalf("snapshot %s: %v", relative, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(store.Root(), "raw", second.RawPath))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "second" {
		t.Fatalf("raw body = %q", body)
	}
	if second.RawPath != "guide.md" {
		t.Fatalf("upload raw path = %q; want original file name without hash directory", second.RawPath)
	}
}

func TestFileStoreRejectsTraversalAndOversize(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background(), "../secret", 10); err == nil {
		t.Fatal("traversal accepted")
	}
	if _, err := store.Mirror(context.Background(), MirrorInput{SourceKey: "large", Content: []byte(strings.Repeat("x", MaxSourceBytes+1))}); err == nil {
		t.Fatal("oversize source accepted")
	}
}

func TestFileStoreEnsureRejectsSymlinkedManagedDirectory(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "raw")); err != nil {
		t.Fatal(err)
	}
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err == nil {
		t.Fatal("symlinked raw directory accepted")
	}
}

func TestFileStoreEnsureDoesNotCreateLegacyRawSourceDirectories(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "manual", "uploads"} {
		if _, err := os.Stat(filepath.Join(store.Root(), "raw", name)); !os.IsNotExist(err) {
			t.Fatalf("legacy raw directory %q exists or could not be checked: %v", name, err)
		}
	}
}

func TestFileStorePublishPage_UsesUnifiedWikiPageTree(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishPage(context.Background(), "source", "sources/guide.md", []byte("source")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishPage(context.Background(), "topic", "topics/topic-guide.md", []byte("topic")); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"wiki/sources/guide.md", "wiki/topics/topic-guide.md"} {
		if _, err := os.Stat(filepath.Join(store.Root(), relative)); err != nil {
			t.Fatalf("published file %q: %v", relative, err)
		}
	}
	for _, relative := range []string{"sources/guide.md", "wiki/guide.md"} {
		if _, err := os.Stat(filepath.Join(store.Root(), relative)); !os.IsNotExist(err) {
			t.Fatalf("unexpected wiki source path %q exists or could not be checked: %v", relative, err)
		}
	}
}

func TestFileStoreSourceSummary_RoundTripsByVersion(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := ChunkSummaryIR{
		Summary:  "article summary",
		Entities: []EntityIR{{Name: "Shared topic", Kind: "topic", Description: "source description"}},
	}
	body := []byte(`{"summary":"article summary","entities":[{"name":"Shared topic","kind":"topic","description":"source description"}],"concepts":[],"facts":[],"conflicts":[]}`)
	if err := store.WriteSourceSummary(context.Background(), 42, body); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadSourceSummary(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary != want.Summary || len(got.Entities) != 1 || got.Entities[0] != want.Entities[0] {
		t.Fatalf("source summary = %+v, want %+v", got, want)
	}
}

func TestFileStoreChunkCache_RoundTripsAndIsTenantScoped(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	key := ChunkCacheKey{TenantID: 7, ContentHash: chunkContentHash("exact text"), PromptVersion: LeafPromptVersion, ModelVersion: "model-v1"}
	want := ChunkSummaryIR{Summary: "summary", Facts: []FactIR{{Statement: "fact", EvidenceStart: 0, EvidenceEnd: 5}}}
	if err := store.WriteChunkCache(context.Background(), key, want); err != nil {
		t.Fatal(err)
	}
	got, err := store.ReadChunkCache(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Summary.Summary != want.Summary || got.ContentHash != key.ContentHash {
		t.Fatalf("cache = %+v, want %+v", got, want)
	}
	otherTenant := key
	otherTenant.TenantID = 8
	if _, err := store.ReadChunkCache(context.Background(), otherTenant); err == nil {
		t.Fatal("cache leaked across tenant keys")
	}
}

func TestFileStoreEnsure_CreatesUnifiedPageTypeDirectories(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"wiki/sources", "wiki/topics"} {
		info, statErr := os.Stat(filepath.Join(store.Root(), relative))
		if statErr != nil || !info.IsDir() {
			t.Fatalf("page directory %q unavailable: info=%v err=%v", relative, info, statErr)
		}
	}
}

func TestSourceRelative_PreservesGitDirectoryShape(t *testing.T) {
	got := sourceRelative(DefaultTenantID, "repo", "repo:1:docs/runbook/guide.md", "docs/runbook/guide.md")
	if got != "repo-1/docs/runbook/guide.md" {
		t.Fatalf("repo relative path = %q; want readable source directory", got)
	}
}

func TestSourceRelative_UploadUsesOriginalFileNameWithoutHashDirectory(t *testing.T) {
	if got := sourceRelative(DefaultTenantID, "upload", "upload:guide.md", "guide.md"); got != "guide.md" {
		t.Fatalf("upload raw path = %q; want guide.md", got)
	}
	if got := sourceRelative(DefaultTenantID, "upload", "upload:设计方案.pdf", "设计方案.pdf"); got != "设计方案.pdf" {
		t.Fatalf("unicode upload raw path = %q; want original filename", got)
	}
}

func TestSourceRelative_PreservesOriginalFileNameForManualSource(t *testing.T) {
	first := sourceRelative(DefaultTenantID, "manual", "manual:one", "guide.md")
	second := sourceRelative(DefaultTenantID, "manual", "manual:two", "guide.md")
	if first != "manual-one/guide.md" || second != "manual-two/guide.md" {
		t.Fatalf("source file names = %q, %q; want original name", first, second)
	}
	if first == second {
		t.Fatalf("different sources share path %q", first)
	}
}

func TestBuildTopicPage_UsesImmutableIdentityPath(t *testing.T) {
	topic := &model.Topic{TopicID: "topic-stable", Title: "Kubernetes 控制面", AliasesJSON: "[]"}
	article := &WikiArticle{Title: topic.Title, Aliases: []string{}}
	page := buildTopicPage(DefaultTenantID, topic, article)
	if page.PageType != model.PageTypeTopic || page.RelativePath != "topics/kubernetes-控制面.md" {
		t.Fatalf("topic page = %+v", page)
	}
}

func TestSplitMarkdown_HardLimitAndRuneBoundary(t *testing.T) {
	input := strings.Repeat("知识段落。", 7000)
	chunks := SplitMarkdown(input)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	for _, chunk := range chunks {
		if len([]rune(chunk.Text)) > HardChunkTokens*charsPerToken {
			t.Fatalf("chunk %d exceeds hard limit", chunk.Ordinal)
		}
		if !strings.Contains(chunk.Text, "知识") {
			t.Fatalf("chunk %d split invalid utf8", chunk.Ordinal)
		}
	}
}

func TestValidateChunkSummary_RejectsUnknownAndInvalidEvidence(t *testing.T) {
	cases := []string{
		`{"summary":"ok","unknown":true}`,
		`{"summary":"ok","entities":[],"concepts":[],"facts":[{"statement":"x","evidence_start":1,"evidence_end":99}],"conflicts":[]}`,
		`{"summary":"ok","entities":[],"concepts":[],"facts":[]}`,
	}
	for _, raw := range cases {
		if _, err := ValidateChunkSummary(raw, "source"); err == nil {
			t.Fatalf("accepted invalid IR: %s", raw)
		}
	}
}
