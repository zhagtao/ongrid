package llm_wiki_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	store "github.com/ongridio/ongrid/internal/manager/data/knowledge/llm_wiki/store"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	pkgerrs "github.com/ongridio/ongrid/internal/pkg/errs"
	"gorm.io/gorm"
)

type summaryStub struct{ calls int }

func (s *summaryStub) Summarize(_ context.Context, instructions, _, text string) (string, error) {
	s.calls++
	if strings.Contains(instructions, "topic planning stage") {
		return plannerResponse(text, "Guide topic", []string{"Guide entity"}, nil), nil
	}
	if strings.Contains(instructions, "article writing stage") {
		return writerResponse(text, "Guide topic"), nil
	}
	return fmt.Sprintf(`{"summary":%q,"entities":[{"name":"Guide entity","kind":"topic","description":"entity description"}],"concepts":[],"facts":[],"conflicts":[]}`, "摘要 "+text), nil
}

type hierarchySummaryStub struct{ calls int }

func (s *hierarchySummaryStub) Summarize(_ context.Context, schema, _ string, text string) (string, error) {
	s.calls++
	if strings.Contains(schema, "topic planning stage") {
		return plannerResponseWithOrdinals(text, "Aggregated topic", nil, nil), nil
	}
	if strings.Contains(schema, "article writing stage") {
		return writerResponse(text, "Aggregated topic"), nil
	}
	if strings.Contains(schema, "## Aggregation mode") {
		return `{"summary":"merged","entities":[{"name":"Aggregated topic","kind":"topic","description":"merged description"}],"concepts":[],"facts":[],"conflicts":[]}`, nil
	}
	name := "Leaf topic A"
	if s.calls > 1 {
		name = "Leaf topic B"
	}
	return fmt.Sprintf(`{"summary":"leaf","entities":[{"name":%q,"kind":"topic","description":"leaf description"}],"concepts":[],"facts":[],"conflicts":[]}`, name), nil
}

type mutableSummaryStub struct{ name string }

func (s *mutableSummaryStub) Summarize(_ context.Context, instructions, _ string, text string) (string, error) {
	if strings.Contains(instructions, "canonical identity matcher") {
		return `{"decision":"distinct","existing_topic_id":"","reason":"the proposed topic has a different identity"}`, nil
	}
	if strings.Contains(instructions, "topic planning stage") {
		return plannerResponse(text, s.name, []string{s.name}, nil), nil
	}
	if strings.Contains(instructions, "article writing stage") {
		return writerResponse(text, s.name), nil
	}
	return fmt.Sprintf(`{"summary":"summary","entities":[{"name":%q,"kind":"topic","description":"description"}],"concepts":[],"facts":[],"conflicts":[]}`, s.name), nil
}

type evidenceSummaryStub struct {
	calls      int
	evidenceID string
}

type crossFileSummaryStub struct{ calls int }

func (s *crossFileSummaryStub) Summarize(_ context.Context, instructions, _, text string) (string, error) {
	s.calls++
	if strings.Contains(instructions, "topic planning stage") {
		return plannerResponse(text, "Shared topic", []string{"Shared topic"}, nil), nil
	}
	if strings.Contains(instructions, "article writing stage") {
		return writerResponse(text, "Shared topic"), nil
	}
	if strings.Contains(instructions, "## Aggregation mode") {
		return `{"summary":"merged summary","entities":[{"name":"Shared topic","kind":"topic","description":"merged description from both articles"}],"concepts":[],"facts":[],"conflicts":[]}`, nil
	}
	description := "description from article A"
	if strings.Contains(text, "article B") {
		description = "description from article B"
	}
	return fmt.Sprintf(`{"summary":"source summary","entities":[{"name":"Shared topic","kind":"topic","description":%q}],"concepts":[],"facts":[],"conflicts":[]}`, description), nil
}

func (s *evidenceSummaryStub) Summarize(_ context.Context, instructions, _, text string) (string, error) {
	s.calls++
	if strings.Contains(instructions, "topic planning stage") {
		return `{"topics":[]}`, nil
	}
	return fmt.Sprintf(`{"summary":"summary","entities":[],"concepts":[],"facts":[{"statement":"fact","evidence_ids":[%q]}],"conflicts":[]}`, s.evidenceID), nil
}

func plannerResponse(input, title string, entities, concepts []string) string {
	return plannerResponseWithOrdinals(input, title, entities, concepts)
}

func plannerResponseWithOrdinals(input, title string, entities, concepts []string) string {
	var payload struct {
		SourceVersionID uint64               `json:"source_version_id"`
		LeafSummaries   []biz.ChunkSummaryIR `json:"leaf_summaries"`
	}
	if err := json.Unmarshal([]byte(input), &payload); err != nil {
		return `{"topics":[]}`
	}
	refs := make([]map[string]any, 0, len(payload.LeafSummaries))
	for ordinal := range payload.LeafSummaries {
		refs = append(refs, map[string]any{"source_version_id": payload.SourceVersionID, "chunk_ordinal": ordinal})
	}
	result := map[string]any{"topics": []map[string]any{{
		"id": "proposal", "canonical_key": title, "title": title, "aliases": []string{},
		"purpose": "grounded topic", "entity_names": entities, "concept_names": concepts,
		"chunk_refs": refs, "required_sections": []string{"概述", "详细信息"}, "importance": 0.95,
	}}}
	body, err := json.Marshal(result)
	if err != nil {
		return `{"topics":[]}`
	}
	return string(body)
}

func writerResponse(input, title string) string {
	var payload struct {
		Evidence []biz.EvidenceItem `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(input), &payload); err != nil || len(payload.Evidence) == 0 {
		return `{"title":"invalid","summary":"invalid","sections":[],"aliases":[],"limitations":[]}`
	}
	refs := []map[string]any{{"source_version_id": payload.Evidence[0].SourceVersionID, "chunk_ordinal": payload.Evidence[0].ChunkOrdinal, "evidence_start": 0, "evidence_end": 0}}
	article := map[string]any{"title": title, "summary": "merged description from both articles", "aliases": []string{}, "limitations": []string{}, "sections": []map[string]any{
		{"title": "概述", "body": "基于来源证据整理的主题概述。", "evidence_refs": refs},
		{"title": "详细信息", "body": "多个来源中的实现细节在这里统一组织。", "evidence_refs": refs},
	}}
	body, err := json.Marshal(article)
	if err != nil {
		return ""
	}
	return string(body)
}

type failingIndexer struct{}

func (failingIndexer) IndexPage(context.Context, biz.IndexDocument) error {
	return errors.New("index offline")
}

func (failingIndexer) DeletePage(context.Context, *model.Page) error {
	return errors.New("index offline")
}

func (failingIndexer) Search(context.Context, uint64, string, int) ([]biz.SearchHit, error) {
	return nil, errors.New("index offline")
}

func TestUploadSource_AcceptsTextAndRejectsUnsupportedFiles(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uc, err := biz.New(context.Background(), repo, files, &summaryStub{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	original := []byte("# Notes\n\nThis is the original upload.")
	var uploaded *model.Source
	for _, filename := range []string{"notes.txt", "notes.text", "notes.md", "notes.markdown"} {
		source, _, err := uc.UploadSource(context.Background(), filename, original)
		if err != nil {
			t.Fatalf("UploadSource(%q): %v", filename, err)
		}
		if uploaded == nil {
			uploaded = source
		}
	}
	raw, err := files.Read(context.Background(), "raw/"+uploaded.RawPath, biz.MaxSourceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(original) {
		t.Fatalf("raw upload = %q, want original %q", raw, original)
	}
	if _, _, err := uc.UploadSource(context.Background(), "notes.xlsx", []byte("data")); !errors.Is(err, pkgerrs.ErrInvalid) {
		t.Fatalf("unsupported upload error = %v", err)
	}
}

func TestDeleteNode_RemovesRawAndRelatedWiki(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uc, err := biz.New(context.Background(), repo, files, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	source, _, err := uc.UploadSource(context.Background(), "guide.md", []byte("# Guide"))
	if err != nil {
		t.Fatal(err)
	}
	page := &model.Page{TenantID: 0, PageID: "page-delete", PageType: model.PageTypeTopic, Title: "Guide", RelativePath: "topics/page-delete.md", AliasesJSON: "[]", Language: "und"}
	if _, err := files.PublishPage(context.Background(), page.PageType, page.RelativePath, []byte("# Guide")); err != nil {
		t.Fatal(err)
	}
	if err := repo.PublishPage(context.Background(), page, []*model.PageSource{{TenantID: 0, PageID: page.PageID, SourceVersionID: *source.CurrentVersionID}}); err != nil {
		t.Fatal(err)
	}

	rawID := base64.RawURLEncoding.EncodeToString([]byte("raw:" + source.RawPath))
	if err := uc.DeleteNode(context.Background(), rawID); err != nil {
		t.Fatalf("delete raw node: %v", err)
	}
	if sources, _, err := repo.ListSources(context.Background(), 0, "", 50); err != nil {
		t.Fatal(err)
	} else if len(sources) != 0 {
		t.Fatalf("sources after delete = %d, want 0", len(sources))
	}
	if pages, err := repo.ListPages(context.Background(), 0); err != nil {
		t.Fatal(err)
	} else if len(pages) != 0 {
		t.Fatalf("pages after delete = %d, want 0", len(pages))
	}
	if _, err := files.Read(context.Background(), "raw/"+source.RawPath, biz.MaxSourceBytes); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("raw file after delete error = %v", err)
	}
}

func TestDeleteNode_WikiKeepsRawSource(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uc, err := biz.New(context.Background(), repo, files, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	source, _, err := uc.UploadSource(context.Background(), "guide.md", []byte("# Guide"))
	if err != nil {
		t.Fatal(err)
	}
	page := &model.Page{TenantID: 0, PageID: "page-wiki-delete", PageType: model.PageTypeTopic, Title: "Guide", RelativePath: "topics/page-wiki-delete.md", AliasesJSON: "[]", Language: "und"}
	if _, err := files.PublishPage(context.Background(), page.PageType, page.RelativePath, []byte("# Guide")); err != nil {
		t.Fatal(err)
	}
	if err := repo.PublishPage(context.Background(), page, []*model.PageSource{{TenantID: 0, PageID: page.PageID, SourceVersionID: *source.CurrentVersionID}}); err != nil {
		t.Fatal(err)
	}

	wikiID := base64.RawURLEncoding.EncodeToString([]byte("wiki:" + page.RelativePath))
	if err := uc.DeleteNode(context.Background(), wikiID); err != nil {
		t.Fatalf("delete wiki node: %v", err)
	}
	if sources, _, err := repo.ListSources(context.Background(), 0, "", 50); err != nil {
		t.Fatal(err)
	} else if len(sources) != 1 {
		t.Fatalf("sources after wiki delete = %d, want 1", len(sources))
	}
	if _, err := files.Read(context.Background(), "raw/"+source.RawPath, biz.MaxSourceBytes); err != nil {
		t.Fatalf("raw file after wiki delete: %v", err)
	}
	if _, err := files.Read(context.Background(), "wiki/"+page.RelativePath, biz.MaxPageBytes); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wiki file after delete error = %v", err)
	}
}

func TestCompileJob_PersistsAndPublishes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &summaryStub{}
	uc, err := biz.New(context.Background(), repo, files, summarizer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, changed, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:1", SourceType: "manual", Name: "guide.md", Content: []byte("# Guide\n\ncontent")})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("first mirror was unchanged")
	}
	if summarizer.calls != 0 {
		t.Fatalf("raw mirror invoked the LLM %d times", summarizer.calls)
	}
	job, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false); !errors.Is(err, pkgerrs.ErrConflict) {
		t.Fatalf("duplicate active job error = %v", err)
	}
	secondSource, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:2", SourceType: "manual", Name: "second.md", Content: []byte("second")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID, secondSource.ID}, false); !errors.Is(err, pkgerrs.ErrConflict) {
		t.Fatalf("overlapping active job error = %v", err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	jobs, _, err := uc.ListJobs(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].ID != job.ID || jobs[0].Status != model.JobSucceeded {
		t.Fatalf("job = %+v", jobs[0])
	}
	if summarizer.calls != 2 {
		t.Fatalf("summarizer calls = %d", summarizer.calls)
	}
	forced, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	got, _, err := uc.ListJobs(context.Background(), 0, 10)
	if err != nil || got[0].ID != forced.ID || got[0].Status != model.JobSucceeded {
		t.Fatalf("forced compile jobs = %+v, err=%v", got, err)
	}
	nodes, err := uc.ListTree(context.Background(), "wiki", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("wiki page not published")
	}
	var topicDirectory *biz.TreeNode
	for i := range nodes {
		if nodes[i].Name == "topics" {
			topicDirectory = &nodes[i]
			break
		}
	}
	if topicDirectory == nil || topicDirectory.DocumentCount != 0 {
		t.Fatalf("topic directory document count = %+v, want 0 before promotion", topicDirectory)
	}
	pages, err := repo.ListPages(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].PageType != model.PageTypeSource {
		t.Fatalf("pages = %+v, want only source page before promotion", pages)
	}
	topics, err := repo.ListTopics(context.Background(), 0)
	if err != nil || len(topics) != 1 || topics[0].Status != model.TopicPending {
		t.Fatalf("pending topics = %+v, err=%v", topics, err)
	}
	rawNodeID := base64.RawURLEncoding.EncodeToString([]byte("raw:" + source.RawPath))
	rawDetail, err := uc.GetNode(context.Background(), rawNodeID)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rawDetail.Metadata["wiki_files"]); !strings.Contains(got, pages[0].PageID) || strings.Contains(got, "topic-") {
		t.Fatalf("wiki_files metadata = %v, want source page without unpromoted topic page", rawDetail.Metadata["wiki_files"])
	}
	_, changed, err = uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:1", SourceType: "manual", Name: "guide.md", Content: []byte("# Guide\n\ncontent")})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("identical mirror created a version")
	}
	otherTenant, _, err := uc.Mirror(context.Background(), biz.MirrorInput{TenantID: 7, SourceKey: "manual:1", SourceType: "manual", Name: "guide.md", Content: []byte("# Other tenant")})
	if err != nil {
		t.Fatal(err)
	}
	if otherTenant.RawPath == source.RawPath {
		t.Fatalf("tenant raw paths collide: %q", source.RawPath)
	}
	updated, changed, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:1", SourceType: "manual", Name: "guide.md", Content: []byte("# Guide\n\nchanged")})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || updated.Status != model.SourceStale {
		t.Fatalf("updated source = %+v, changed=%v", updated, changed)
	}
}

func TestCompileJob_UsesHierarchySummaryForDerivedPages(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &hierarchySummaryStub{}
	uc, err := biz.New(context.Background(), repo, files, summarizer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := uc.Mirror(context.Background(), biz.MirrorInput{
		SourceKey:  "manual:hierarchy",
		SourceType: "manual",
		Name:       "hierarchy.md",
		Content:    []byte(strings.Repeat("source ", 3600)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	if summarizer.calls != 5 {
		t.Fatalf("summarizer calls = %d, want leaves, hierarchy, planner, and writer", summarizer.calls)
	}
	pages, err := repo.ListPages(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("published pages = %d, want source plus one aggregated page", len(pages))
	}
	for _, page := range pages {
		if page.PageType == model.PageTypeTopic && page.Title != "Aggregated topic" {
			t.Fatalf("unexpected derived page: %+v", page)
		}
		if page.Title == "Leaf topic A" || page.Title == "Leaf topic B" {
			t.Fatalf("leaf summary escaped into derived pages: %+v", page)
		}
	}
}

func TestCompileJob_MergesSameDerivedPageAcrossSequentialUploads(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &crossFileSummaryStub{}
	uc, err := biz.New(context.Background(), repo, files, summarizer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	first, _, err := uc.UploadSource(context.Background(), "article-a.md", []byte("article A discusses the shared topic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{first.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}

	second, _, err := uc.UploadSource(context.Background(), "article-b.md", []byte("article B adds another view of the shared topic"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{second.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}

	if summarizer.calls != 5 {
		t.Fatalf("summarizer calls = %d, want two extractors, two planners, and one writer", summarizer.calls)
	}
	pages, err := repo.ListPages(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	var sharedPage *model.Page
	for _, page := range pages {
		if page.PageType == model.PageTypeTopic && page.Title == "Shared topic" {
			sharedPage = page
			break
		}
	}
	if sharedPage == nil {
		t.Fatal("shared derived page not published")
	}
	refs, err := repo.ListPageSources(context.Background(), 0, sharedPage.PageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 2 {
		t.Fatalf("shared page source refs = %+v, want two versions", refs)
	}
	_, body, err := uc.ReadPage(context.Background(), 0, sharedPage.PageID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "merged description from both articles") {
		t.Fatalf("shared page body = %q, want merged description", body)
	}
	if !strings.Contains(body, fmt.Sprintf("source_versions: [%d, %d]", *first.CurrentVersionID, *second.CurrentVersionID)) {
		t.Fatalf("shared page body = %q, want both source versions", body)
	}
}

func TestCompileJob_RemovesStaleDerivedPagesOnRecompile(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &mutableSummaryStub{name: "Old topic"}
	uc, err := biz.New(context.Background(), repo, files, summarizer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:stale", SourceType: "manual", Name: "stale.md", Content: []byte("old content")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}

	summarizer.name = "New topic"
	if _, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:stale", SourceType: "manual", Name: "stale.md", Content: []byte("new content")}); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}

	pages, err := repo.ListPages(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || pages[0].PageType != model.PageTypeSource {
		t.Fatalf("pages after recompile = %+v, want only source while topic is below promotion gate", pages)
	}
	topics, err := repo.ListTopics(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 || topics[0].Title != "New topic" {
		t.Fatalf("topics after recompile = %+v, want only current topic", topics)
	}
}

func TestCompileJob_FailsWhenEvidenceIsNotInSource(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &evidenceSummaryStub{evidenceID: "missing"}
	uc, err := biz.New(context.Background(), repo, files, summarizer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:empty-repair", SourceType: "manual", Name: "empty-repair.md", Content: []byte("short source")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err == nil {
		t.Fatal("compile accepted an unknown evidence id")
	}
	if summarizer.calls != 1 {
		t.Fatalf("summarizer calls = %d, want extractor only", summarizer.calls)
	}
}

func TestCompileJob_RequestTriggerRunsOnce(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	summarizer := &summaryStub{}
	uc, err := biz.New(runCtx, repo, files, summarizer, nil, nil, biz.CompileTriggerOption{Owner: "test-trigger", Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "trigger:1", SourceType: "manual", Name: "trigger.md", Content: []byte("# Trigger\n\ncontent")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		got, getErr := repo.GetJob(context.Background(), 0, job.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status == model.JobSucceeded {
			if summarizer.calls != 2 {
				t.Fatalf("summarizer calls = %d, want 2", summarizer.calls)
			}
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("job did not finish: %+v", got)
		case <-ticker.C:
		}
	}
}

func TestCompileJob_ResolvesFactEvidenceWithoutRetry(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	summarizer := &evidenceSummaryStub{evidenceID: "e0"}
	uc, err := biz.New(context.Background(), repo, files, summarizer, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "repair:1", SourceType: "manual", Name: "repair.md", Content: []byte("# Repair\n\ncontent")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false); err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "test-worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	jobs, _, err := uc.ListJobs(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].Status != model.JobSucceeded {
		t.Fatalf("jobs = %+v", jobs)
	}
	if summarizer.calls != 2 {
		t.Fatalf("summarizer calls = %d, want extractor plus planner", summarizer.calls)
	}
}

func TestNew_ReconcilesCompletedPublishManifest(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uc, err := biz.New(context.Background(), repo, files, &summaryStub{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:reconcile", SourceType: "manual", Name: "recover.md", Content: []byte("recover")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nid: source-recovered\ntype: source\ntitle: Recovered\naliases: []\nlanguage: und\nsource_versions: [1]\nrelated: []\n---\n\n# Recovered\n")
	digest := sha256.Sum256(body)
	page := &model.Page{PageID: "source-recovered", TenantID: 0, PageType: "source", Title: "Recovered", AliasesJSON: "[]", Language: "und", RelativePath: "manual/recovered.md", BodySHA256: hex.EncodeToString(digest[:])}
	if _, err := files.PublishPage(context.Background(), page.PageType, page.RelativePath, body); err != nil {
		t.Fatal(err)
	}
	if err := files.StageManifest(context.Background(), biz.PublishManifest{JobID: job.ID, TenantID: 0, SourceID: source.ID, VersionID: *source.CurrentVersionID, Pages: []*model.Page{page}}); err != nil {
		t.Fatal(err)
	}
	if _, err := biz.New(context.Background(), repo, files, &summaryStub{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetJob(context.Background(), 0, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobSucceeded || got.Stage != "reconciled" {
		t.Fatalf("reconciled job = %+v", got)
	}
}

func TestFileStore_RejectsLegacyEntityPageType(t *testing.T) {
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := files.PublishPage(context.Background(), "entity", "entities/legacy.md", []byte("legacy")); err == nil {
		t.Fatal("legacy entity page type was accepted")
	}
}

func TestCompileJob_IndexFailureKeepsPublishedPage(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := store.New(db)
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uc, err := biz.New(context.Background(), repo, files, &summaryStub{}, failingIndexer{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, _, err := uc.Mirror(context.Background(), biz.MirrorInput{SourceKey: "manual:index-failure", SourceType: "manual", Name: "guide.md", Content: []byte("# Guide")})
	if err != nil {
		t.Fatal(err)
	}
	job, err := uc.CreateCompileJob(context.Background(), 0, []uint64{source.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := uc.RunOnce(context.Background(), 0, "worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetJob(context.Background(), 0, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != model.JobSucceeded || got.Stage != "index_failed" {
		t.Fatalf("job = %+v", got)
	}
	pages, err := repo.ListPages(context.Background(), 0)
	if err != nil || len(pages) == 0 {
		t.Fatalf("published pages = %+v, err=%v", pages, err)
	}
	if _, _, err := uc.ReadPage(context.Background(), 0, pages[0].PageID); err != nil {
		t.Fatalf("read published page: %v", err)
	}
}
