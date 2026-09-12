package llm_wiki

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
)

func TestSQLiteStore_SecureSingleDatabasePersistsState(t *testing.T) {
	root := t.TempDir()
	files, err := biz.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := files.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenSQLiteStore(context.Background(), root, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	var journalMode string
	if err := opened.DB.Raw("PRAGMA journal_mode").Scan(&journalMode).Error; err != nil {
		t.Fatal(err)
	}
	var foreignKeys, busyTimeout int
	if err := opened.DB.Raw("PRAGMA foreign_keys").Scan(&foreignKeys).Error; err != nil {
		t.Fatal(err)
	}
	if err := opened.DB.Raw("PRAGMA busy_timeout").Scan(&busyTimeout).Error; err != nil {
		t.Fatal(err)
	}
	if journalMode != "wal" || foreignKeys != 1 || busyTimeout != 5000 {
		t.Fatalf("journal=%q foreign_keys=%d busy_timeout=%d", journalMode, foreignKeys, busyTimeout)
	}
	dbPath := filepath.Join(root, ".llm-wiki", "wiki.db")
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("wiki.db mode = %o", info.Mode().Perm())
	}
	if err := opened.DB.Create(&model.Source{SourceKey: "manual:persistent", SourceType: "manual", RawPath: "persistent.md", Status: model.SourcePending}).Error; err != nil {
		t.Fatal(err)
	}
	var legacyTables int64
	if err := opened.DB.Raw("SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'knowledge_wiki_%'").Scan(&legacyTables).Error; err != nil {
		t.Fatal(err)
	}
	if legacyTables != 0 {
		t.Fatalf("legacy LLM Wiki tables = %d", legacyTables)
	}
	firstPool := opened.sqlDB
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstPool.Ping(); err == nil {
		t.Fatal("closed SQLite pool still accepts Ping")
	}

	reopened, err := OpenSQLiteStore(context.Background(), root, nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	}()
	var count int64
	if err := reopened.DB.Model(&model.Source{}).Where("source_key = ?", "manual:persistent").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("persistent source count = %d", count)
	}
}
