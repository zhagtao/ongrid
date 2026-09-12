package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
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

func TestPublishPage_PopulatesPathHash(t *testing.T) {
	repo, db := testRepo(t)
	page := &model.Page{
		PageID:       "page-hash",
		TenantID:     7,
		PageType:     "source",
		Title:        "Long path",
		AliasesJSON:  "[]",
		Language:     "und",
		RelativePath: strings.Repeat("长", 1024),
	}
	if err := repo.PublishPage(context.Background(), page, nil); err != nil {
		t.Fatal(err)
	}
	pathSum := sha256.Sum256([]byte(page.RelativePath))
	want := hex.EncodeToString(pathSum[:])
	if page.RelativePathSHA256 == nil || *page.RelativePathSHA256 != want {
		t.Fatalf("relative path hash = %v, want %q", page.RelativePathSHA256, want)
	}
	var stored model.Page
	if err := db.First(&stored, "page_id = ?", page.PageID).Error; err != nil {
		t.Fatal(err)
	}
	if page.ID == 0 || stored.ID != page.ID {
		t.Fatalf("page auto id = %d, stored id = %d", page.ID, stored.ID)
	}
	if stored.RelativePathSHA256 == nil || *stored.RelativePathSHA256 != want {
		t.Fatalf("stored relative path hash = %v, want %q", stored.RelativePathSHA256, want)
	}
}

func TestClaimJob_WhenLeaseExpires_RecoversAfterRestart(t *testing.T) {
	repo, db := testRepo(t)
	source := &model.Source{TenantID: 0, SourceKey: "source:lease", SourceType: "manual", RawPath: "manual/lease.md", Status: model.SourcePending}
	if err := db.Create(source).Error; err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal([]uint64{source.ID})
	job := &model.CompileJob{TenantID: 0, SourceIDsJSON: string(body), Status: model.JobPending, Stage: "queued"}
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

func TestCancelJob_ReleasesRunningSourceLock(t *testing.T) {
	repo, db := testRepo(t)
	source := &model.Source{TenantID: 0, SourceKey: "source:cancel", SourceType: "manual", RawPath: "manual/cancel.md", Status: model.SourcePending}
	if err := db.Create(source).Error; err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]uint64{source.ID})
	if err != nil {
		t.Fatal(err)
	}
	activeKey := "cancel-key"
	job := &model.CompileJob{TenantID: 0, SourceIDsJSON: string(body), ActiveKey: &activeKey, Status: model.JobPending, Stage: "queued"}
	if err := repo.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClaimJob(context.Background(), 0, "worker", time.Minute); err != nil {
		t.Fatal(err)
	}
	cancelled, err := repo.CancelJob(context.Background(), 0, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != model.JobCancelled {
		t.Fatalf("cancelled job = %+v", cancelled)
	}
	var got model.Source
	if err := db.First(&got, source.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ActiveJobID != nil {
		t.Fatalf("source lock remains: %+v", got.ActiveJobID)
	}
}

func TestCommitTopicCompilation_PersistsStableTopicAndSupersedesSourceEvidence(t *testing.T) {
	repo, db := testRepo(t)
	ctx := context.Background()
	source := &model.Source{TenantID: 0, SourceKey: "source:topic", SourceType: "manual", RawPath: "manual/topic.md", Status: model.SourcePending}
	if err := db.Create(source).Error; err != nil {
		t.Fatal(err)
	}
	v1 := &model.SourceVersion{TenantID: 0, SourceID: source.ID, SHA256: "v1", SnapshotPath: "v1.md", SchemaVersion: "v1"}
	v2 := &model.SourceVersion{TenantID: 0, SourceID: source.ID, SHA256: "v2", SnapshotPath: "v2.md", SchemaVersion: "v1"}
	if err := db.Create(v1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(v2).Error; err != nil {
		t.Fatal(err)
	}
	topic := &model.Topic{TopicID: "topic-stable", TenantID: 0, CanonicalKey: "stable", Title: "Stable", AliasesJSON: `[]`, EntityNamesJSON: `[]`, ConceptNamesJSON: `[]`, Status: model.TopicPublished}
	page := &model.Page{PageID: topic.TopicID, TenantID: 0, PageType: model.PageTypeTopic, Title: topic.Title, AliasesJSON: `[]`, Language: "und", RelativePath: "topics/topic-stable.md"}
	commit := func(versionID uint64) error {
		evidence := []*model.TopicEvidence{{TenantID: 0, TopicID: topic.TopicID, SourceID: source.ID, SourceVersionID: versionID, ChunkOrdinal: 0, Kind: "context"}}
		batch := biz.ArtifactBatch{Pages: []biz.WikiPage{{ID: page.PageID, Type: page.PageType, Title: page.Title, Aliases: []string{}, Language: page.Language, RelativePath: page.RelativePath}}, PageSourceVersionIDs: map[string][]uint64{page.PageID: {versionID}}}
		return repo.CommitTopicCompilation(ctx, 0, source.ID, versionID, "source-page", []*model.Topic{topic}, evidence, batch)
	}
	if err := commit(v1.ID); err != nil {
		t.Fatal(err)
	}
	if err := commit(v2.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListTopicEvidence(ctx, 0, topic.TopicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SourceVersionID != v2.ID {
		t.Fatalf("evidence = %+v, want only v2", rows)
	}
	stored, err := repo.GetTopic(ctx, 0, topic.TopicID)
	if err != nil || stored.TopicID != topic.TopicID {
		t.Fatalf("topic = %+v, err=%v", stored, err)
	}
}

func TestCommitTopicCompilation_RollsBackTopicOnEvidenceFailure(t *testing.T) {
	repo, _ := testRepo(t)
	topic := &model.Topic{TopicID: "topic-rollback", TenantID: 0, CanonicalKey: "rollback", Title: "Rollback", AliasesJSON: `[]`, EntityNamesJSON: `[]`, ConceptNamesJSON: `[]`, Status: model.TopicPublished}
	row := &model.TopicEvidence{TenantID: 0, TopicID: topic.TopicID, SourceID: 1, SourceVersionID: 1, ChunkOrdinal: 0, Kind: "context"}
	err := repo.CommitTopicCompilation(context.Background(), 0, 1, 1, "source-page", []*model.Topic{topic}, []*model.TopicEvidence{row, row}, biz.ArtifactBatch{})
	if err == nil {
		t.Fatal("duplicate evidence commit succeeded")
	}
	if _, getErr := repo.GetTopic(context.Background(), 0, topic.TopicID); !errors.Is(getErr, errs.ErrNotFound) {
		t.Fatalf("topic survived rolled-back commit: %v", getErr)
	}
}

func TestDeleteSource_PreservesSharedTopicMetadataAndOtherEvidence(t *testing.T) {
	repo, db := testRepo(t)
	ctx := context.Background()
	sourceA := &model.Source{TenantID: 0, SourceKey: "source:delete-a", SourceType: "manual", RawPath: "manual/delete-a.md", Status: model.SourceSucceeded}
	sourceB := &model.Source{TenantID: 0, SourceKey: "source:delete-b", SourceType: "manual", RawPath: "manual/delete-b.md", Status: model.SourceSucceeded}
	if err := db.Create(sourceA).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(sourceB).Error; err != nil {
		t.Fatal(err)
	}
	versionA := &model.SourceVersion{TenantID: 0, SourceID: sourceA.ID, SHA256: "delete-a-v1", SnapshotPath: "delete-a-v1.md", SchemaVersion: biz.SchemaVersion}
	versionB := &model.SourceVersion{TenantID: 0, SourceID: sourceB.ID, SHA256: "delete-b-v1", SnapshotPath: "delete-b-v1.md", SchemaVersion: biz.SchemaVersion}
	if err := db.Create(versionA).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(versionB).Error; err != nil {
		t.Fatal(err)
	}
	topic := &model.Topic{TopicID: "topic-shared-delete", TenantID: 0, CanonicalKey: "shared-delete", Title: "Shared delete", AliasesJSON: `[]`, EntityNamesJSON: `[]`, ConceptNamesJSON: `[]`, Summary: "summary", Status: model.TopicPublished}
	page := &model.Page{PageID: topic.TopicID, TenantID: 0, PageType: model.PageTypeTopic, Title: topic.Title, AliasesJSON: `[]`, Language: "und", RelativePath: "topics/shared-delete.md"}
	if err := db.Create(topic).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(page).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create([]*model.TopicEvidence{
		{TenantID: 0, TopicID: topic.TopicID, SourceID: sourceA.ID, SourceVersionID: versionA.ID, ChunkOrdinal: 0, Kind: "context"},
		{TenantID: 0, TopicID: topic.TopicID, SourceID: sourceB.ID, SourceVersionID: versionB.ID, ChunkOrdinal: 0, Kind: "context"},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create([]*model.PageSource{
		{TenantID: 0, PageID: page.PageID, SourceVersionID: versionA.ID},
		{TenantID: 0, PageID: page.PageID, SourceVersionID: versionB.ID},
	}).Error; err != nil {
		t.Fatal(err)
	}

	if err := repo.DeleteSource(ctx, 0, sourceA.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListTopicEvidence(ctx, 0, topic.TopicID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SourceID != sourceB.ID {
		t.Fatalf("remaining evidence = %+v, want source B only", rows)
	}
	stored, err := repo.GetTopic(ctx, 0, topic.TopicID)
	if err != nil || stored.Status != model.TopicPending {
		t.Fatalf("shared topic = %+v, err=%v; want pending metadata", stored, err)
	}
	pages, err := repo.ListPages(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 0 {
		t.Fatalf("stale shared page = %+v, want unpublished", pages)
	}
}
