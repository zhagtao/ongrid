package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	store "github.com/ongridio/ongrid/internal/manager/data/knowledge/llm_wiki/store"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"gorm.io/gorm"
)

type indexEmbedder struct {
	calls int
	err   error
}

func (*indexEmbedder) Dim() int { return 2 }

func (e *indexEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	e.calls++
	if e.err != nil {
		return nil, e.err
	}
	return [][]float32{{1, 0}}, nil
}

func TestIndex_SearchesFilesAndIsolatesTenant(t *testing.T) {
	root := t.TempDir()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Page{}, &model.Topic{}); err != nil {
		t.Fatal(err)
	}
	pages := []*model.Page{
		{PageID: "dns", TenantID: 1, PageType: model.PageTypeTopic, Title: "DNS 排障", RelativePath: "topics/dns.md", AliasesJSON: "[]", Language: "und"},
		{PageID: "dns-other", TenantID: 2, PageType: model.PageTypeTopic, Title: "DNS", RelativePath: "topics/dns-other.md", AliasesJSON: "[]", Language: "und"},
	}
	for _, page := range pages {
		if err := db.Create(page).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.Topic{TopicID: page.PageID, TenantID: page.TenantID, CanonicalKey: page.PageID, Title: page.Title, AliasesJSON: "[]", EntityNamesJSON: "[]", ConceptNamesJSON: "[]", Summary: "", Status: model.TopicPublished}).Error; err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, "wiki", filepath.FromSlash(page.RelativePath))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(map[string]string{"dns": "检查解析超时", "dns-other": "另一个租户的解析文档"}[page.PageID]), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := New(context.Background(), root, db, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, document := range []biz.IndexDocument{{Page: pages[0], Content: "检查解析超时", SourceVersionIDs: []uint64{11}}, {Page: pages[1], Content: "另一个租户的解析文档", SourceVersionIDs: []uint64{22}}} {
		if err := idx.IndexPage(context.Background(), document); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := idx.Search(context.Background(), 1, "解析", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].PageID != "dns" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestIndex_RebuildPreservesDurableState(t *testing.T) {
	root := t.TempDir()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Source{TenantID: 0, SourceKey: "manual:design", SourceType: "manual", RawPath: "design.md", Status: model.SourceSucceeded}).Error; err != nil {
		t.Fatal(err)
	}
	files, err := biz.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	pageBodies := map[string]string{
		"sources/design.md": "---\nid: source-design\ntype: source\ntitle: Design\naliases: []\nlanguage: zh\nsource_versions: []\nrelated: [topics/search]\n---\n\n# Design\n本地搜索",
		"topics/search.md":  "---\nid: topic-search\ntype: topic\ntitle: Search\naliases: [检索]\nlanguage: zh\nsource_versions: []\nrelated: []\n---\n\n# Search\n支持中英文检索",
	}
	for relative, body := range pageBodies {
		pageType := model.PageTypeSource
		if filepath.Dir(relative) == "topics" {
			pageType = model.PageTypeTopic
		}
		if _, err := files.PublishPage(context.Background(), pageType, relative, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := New(context.Background(), root, db, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := idx.Rebuild(context.Background(), 0, files)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %+v", warnings)
	}
	var sourceCount, pageCount, linkCount int64
	if err := db.Model(&model.Source{}).Count(&sourceCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.Page{}).Count(&pageCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.PageRelation{}).Count(&linkCount).Error; err != nil {
		t.Fatal(err)
	}
	if sourceCount != 1 || pageCount != 2 || linkCount != 1 {
		t.Fatalf("sources=%d pages=%d links=%d", sourceCount, pageCount, linkCount)
	}
	hits, err := idx.Search(context.Background(), 0, "检索", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].PageID != "topic-search" {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestIndex_ReusesBodyHashAndDropsStaleVectorOnEmbeddingFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(db); err != nil {
		t.Fatal(err)
	}
	embedder := &indexEmbedder{}
	idx, err := New(context.Background(), t.TempDir(), db, embedder, 2)
	if err != nil {
		t.Fatal(err)
	}
	page := &model.Page{PageID: "hash-page", PageType: model.PageTypeTopic, Title: "Hash", AliasesJSON: "[]"}
	document := biz.IndexDocument{Page: page, Content: "稳定正文"}
	if err := idx.IndexPage(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	if err := idx.IndexPage(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	if embedder.calls != 1 {
		t.Fatalf("embedding calls = %d; want body-hash reuse", embedder.calls)
	}
	embedder.err = errors.New("embedding offline")
	document.Content = "更新后的正文"
	if err := idx.IndexPage(context.Background(), document); err == nil {
		t.Fatal("embedding failure was ignored")
	}
	var vectorCount int64
	if err := db.Model(&wikiVector{}).Count(&vectorCount).Error; err != nil {
		t.Fatal(err)
	}
	if vectorCount != 0 {
		t.Fatalf("stale vector count = %d", vectorCount)
	}
	hits, err := idx.searchLexical(context.Background(), 0, "更新", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].PageID != page.PageID {
		t.Fatalf("FTS fallback hits = %+v", hits)
	}
}
