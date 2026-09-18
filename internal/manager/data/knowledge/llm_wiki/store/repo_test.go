package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	biz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/errs"
	"gorm.io/gorm"
)

func testRepo(t *testing.T) (*Repo, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	repo := New(db)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	return repo, db
}

func TestClaimJob_WhenLeaseExpires_RecoversAfterRestart(t *testing.T) {
	repo, db := testRepo(t)
	activeKey := "tenant-0-full-corpus"
	job := &model.CompileJob{TenantID: 0, ActiveKey: &activeKey, Status: model.JobPending, Stage: "queued"}
	if err := repo.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	claimed, err := repo.ClaimJob(context.Background(), 0, "worker-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.LeaseOwner != "worker-a" {
		t.Fatalf("owner = %q", claimed.LeaseOwner)
	}
	if _, err := repo.ClaimJob(context.Background(), 0, "worker-b", time.Minute); !errors.Is(err, biz.ErrNoPendingJob) {
		t.Fatalf("active lease claim error = %v", err)
	}
	past := time.Now().Add(-time.Minute)
	if err := db.Model(&model.CompileJob{}).Where("id = ?", job.ID).Update("lease_expires_at", past).Error; err != nil {
		t.Fatal(err)
	}
	recovered, err := repo.ClaimJob(context.Background(), 0, "worker-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LeaseOwner != "worker-b" || recovered.Attempt != 2 {
		t.Fatalf("recovered = %+v", recovered)
	}
}

func TestCreateJob_RejectsSecondActiveTenantJob(t *testing.T) {
	repo, _ := testRepo(t)
	firstKey, secondKey := "first", "second"
	if err := repo.CreateJob(context.Background(), &model.CompileJob{TenantID: 7, ActiveKey: &firstKey, Status: model.JobPending, Stage: "queued"}); err != nil {
		t.Fatal(err)
	}
	err := repo.CreateJob(context.Background(), &model.CompileJob{TenantID: 7, ActiveKey: &secondKey, Status: model.JobPending, Stage: "queued"})
	if !errors.Is(err, errs.ErrConflict) {
		t.Fatalf("second job error = %v", err)
	}
}

func TestCancelJob_AllowsFailedJob(t *testing.T) {
	repo, db := testRepo(t)
	activeKey := "failed-cancel-key"
	job := &model.CompileJob{TenantID: 0, ActiveKey: &activeKey, Status: model.JobFailed, Stage: "compile", ErrorMessage: "compile failed"}
	if err := db.Create(job).Error; err != nil {
		t.Fatal(err)
	}
	cancelled, err := repo.CancelJob(context.Background(), 0, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != model.JobCancelled || !cancelled.CancelRequested || cancelled.ActiveKey != nil {
		t.Fatalf("cancelled job = %+v", cancelled)
	}
}

func TestDeleteSource_RemovesOwnedVersionsAndChunks(t *testing.T) {
	repo, db := testRepo(t)
	source := &model.Source{TenantID: 0, SourceKey: "source:delete", SourceType: "organization", RawPath: "team/delete.md", Status: model.SourceSucceeded}
	if err := db.Create(source).Error; err != nil {
		t.Fatal(err)
	}
	version := &model.SourceVersion{TenantID: 0, SourceID: source.ID, SHA256: "delete-v1", SnapshotPath: "delete-v1.md", SchemaVersion: biz.SchemaVersion}
	if err := db.Create(version).Error; err != nil {
		t.Fatal(err)
	}
	chunk := &model.SourceChunk{TenantID: 0, VersionID: version.ID, Ordinal: 0, SummaryPath: "summary.md", SummarySHA256: "hash"}
	if err := db.Create(chunk).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteSource(context.Background(), 0, source.ID); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]any{"sources": &model.Source{}, "versions": &model.SourceVersion{}, "chunks": &model.SourceChunk{}} {
		var count int64
		if err := db.Model(target).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d", name, count)
		}
	}
}

func TestMigrate_ReplacesLegacyWikiBuildsSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE wiki_builds (id INTEGER PRIMARY KEY, status TEXT NOT NULL DEFAULT 'building', corpus_fingerprint TEXT NOT NULL DEFAULT '', published_at DATETIME)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasTable("wiki_builds") || !db.Migrator().HasColumn("wiki_builds", "tenant_id") {
		t.Fatal("wiki_builds was not recreated with the current schema")
	}
	if db.Migrator().HasColumn("wiki_builds", "corpus_fingerprint") {
		t.Fatal("retired wiki_builds columns still exist")
	}
}
