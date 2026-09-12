package llm_wiki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
)

func TestCatalog_RendersDeterministicIndexAndIdempotentLog(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	source := `---
id: source-design
type: source
title: Design.pdf
description: 系统设计文档
aliases: []
language: zh
source_versions: [12]
related: [topics/transformer, Transformer, Attention model, missing, ../../secret]
---

# Design

[[topics/transformer|Transformer]] [[sources/design|self]]
`
	topic := `---
id: topic-transformer
type: topic
title: Transformer
description: 基于注意力机制的模型架构
aliases: [Attention model]
language: zh
source_versions: [12]
related: []
---

# Transformer
`
	if _, err := store.PublishPage(ctx, "source", "sources/design.md", []byte(source)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishPage(ctx, "topic", "topics/transformer.md", []byte(topic)); err != nil {
		t.Fatal(err)
	}
	event := WikiLogEvent{ID: "compile-job-42", OccurredAt: time.Date(2026, 9, 12, 8, 30, 0, 0, time.UTC), Action: "compile", Subject: "design.pdf?token=secret", SourceVersionID: 12, CreatedPages: 2, Topics: []WikiLogTopic{{Path: "topics/transformer.md", Title: "Transformer"}}}
	pages, links, warnings, err := store.RefreshCatalogFiles(ctx, event)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || len(links) != 1 || len(warnings) != 2 {
		t.Fatalf("pages=%d links=%+v warnings=%+v", len(pages), links, warnings)
	}
	indexPath := filepath.Join(store.Root(), "wiki", "index.md")
	firstIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstIndex), "[[sources/design|Design.pdf]] — 系统设计文档") || !strings.Contains(string(firstIndex), "[[topics/transformer|Transformer]]") {
		t.Fatalf("index = %s", firstIndex)
	}
	if _, _, _, err := store.RefreshCatalogFiles(ctx, event); err != nil {
		t.Fatal(err)
	}
	newer := WikiLogEvent{ID: "delete-topic-43", OccurredAt: event.OccurredAt.Add(time.Minute), Action: "delete-topic", Subject: "Old Topic", DeletedPages: 1}
	if _, _, _, err := store.RefreshCatalogFiles(ctx, newer); err != nil {
		t.Fatal(err)
	}
	secondIndex, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstIndex) != string(secondIndex) {
		t.Fatal("deterministic index changed for the same page set")
	}
	logBody, err := os.ReadFile(filepath.Join(store.Root(), "wiki", "log.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(logBody), "<!-- event: compile-job-42 -->") != 1 {
		t.Fatalf("log event is not idempotent: %s", logBody)
	}
	if strings.Index(string(logBody), "delete-topic-43") > strings.Index(string(logBody), "compile-job-42") {
		t.Fatalf("newest event is not first: %s", logBody)
	}
	if strings.Contains(string(logBody), "secret") {
		t.Fatalf("log leaked URL query: %s", logBody)
	}
}

func TestFileStore_StagesBeforeAtomicActivation(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	body := []byte("---\nid: topic-staged\ntype: topic\ntitle: Staged\naliases: []\nlanguage: und\nrelated: []\n---\n\n# Staged\n")
	sum := sha256.Sum256(body)
	page := &model.Page{PageID: "topic-staged", PageType: model.PageTypeTopic, RelativePath: "topics/staged.md", BodySHA256: hex.EncodeToString(sum[:])}
	manifest := PublishManifest{JobID: 7, Pages: []*model.Page{page}}
	if err := store.StageManifest(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StagePage(ctx, manifest.JobID, page.PageType, page.RelativePath, body); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Root(), "wiki", "topics", "staged.md")); !os.IsNotExist(err) {
		t.Fatalf("candidate became visible before activation: %v", err)
	}
	if err := store.ActivateManifest(ctx, manifest); err != nil {
		t.Fatal(err)
	}
	active, err := os.ReadFile(filepath.Join(store.Root(), "wiki", "topics", "staged.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(active) != string(body) {
		t.Fatalf("active body = %q", active)
	}
}

func TestFileStoreEnsure_CreatesProtectedWikiLayout(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"wiki/index.md", "wiki/log.md"} {
		info, err := os.Stat(filepath.Join(store.Root(), filepath.FromSlash(relative)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o640 {
			t.Fatalf("%s mode = %o", relative, info.Mode().Perm())
		}
	}
	info, err := os.Stat(filepath.Join(store.Root(), ".llm-wiki"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o750 {
		t.Fatalf(".llm-wiki mode = %o", info.Mode().Perm())
	}
}
