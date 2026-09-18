// Package llm_wiki contains persistence entities for the LLM Wiki bounded context.
package llm_wiki

import "time"

const (
	SourcePending   = "pending"
	SourceSucceeded = "succeeded"
	SourceStale     = "stale"
	SourceFailed    = "failed"

	JobPending   = "pending"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobSkipped   = "skipped"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

type Source struct {
	ID               uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID         uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_source,priority:1;index:idx_wiki_source_status,priority:1"`
	SourceKey        string     `gorm:"column:source_key;size:512;not null;default:'';uniqueIndex:uk_wiki_source,priority:2"`
	SourceType       string     `gorm:"column:source_type;size:32;not null;default:''"`
	RawPath          string     `gorm:"column:raw_path;size:1024;not null;default:''"`
	CurrentVersionID *uint64    `gorm:"column:current_version_id"`
	ContentSHA256    string     `gorm:"column:content_sha256;size:64;not null;default:''"`
	Status           string     `gorm:"column:status;size:24;not null;default:'pending';index:idx_wiki_source_status,priority:2"`
	CreatedAt        time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt        *time.Time `gorm:"column:deleted_at;index:idx_wiki_source_deleted"`
}

func (Source) TableName() string { return "wiki_sources" }

type SourceVersion struct {
	ID            uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID      uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_source_version,priority:1;index:idx_wiki_version_source,priority:1"`
	SourceID      uint64     `gorm:"column:source_id;not null;uniqueIndex:uk_wiki_source_version,priority:2;index:idx_wiki_version_source,priority:2"`
	SHA256        string     `gorm:"column:sha256;size:64;not null;default:'';uniqueIndex:uk_wiki_source_version,priority:3"`
	SizeBytes     uint64     `gorm:"column:size_bytes;not null;default:0"`
	SnapshotPath  string     `gorm:"column:snapshot_path;size:1024;not null;default:''"`
	SchemaVersion string     `gorm:"column:schema_version;size:32;not null;default:'v1'"`
	CreatedAt     time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt     *time.Time `gorm:"column:deleted_at;index:idx_wiki_version_deleted"`
}

func (SourceVersion) TableName() string { return "wiki_source_versions" }

type SourceChunk struct {
	ID            uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID      uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_chunk,priority:1;index:idx_wiki_chunk_parent,priority:1"`
	VersionID     uint64     `gorm:"column:version_id;not null;uniqueIndex:uk_wiki_chunk,priority:2"`
	ParentChunkID *uint64    `gorm:"column:parent_chunk_id;index:idx_wiki_chunk_parent,priority:2"`
	Ordinal       uint32     `gorm:"column:ordinal;not null;default:0;uniqueIndex:uk_wiki_chunk,priority:3"`
	StartOffset   uint64     `gorm:"column:start_offset;not null;default:0"`
	EndOffset     uint64     `gorm:"column:end_offset;not null;default:0"`
	SummaryPath   string     `gorm:"column:summary_path;size:1024;not null;default:''"`
	SummarySHA256 string     `gorm:"column:summary_sha256;size:64;not null;default:''"`
	CreatedAt     time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt     *time.Time `gorm:"column:deleted_at;index:idx_wiki_chunk_deleted"`
}

func (SourceChunk) TableName() string { return "wiki_source_chunks" }

type CompileJob struct {
	ID              uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID        uint64     `gorm:"column:tenant_id;not null;default:0;index:idx_wiki_job_claim,priority:1"`
	ActiveKey       *string    `gorm:"column:active_key;size:64;uniqueIndex:uk_wiki_job_active"`
	Status          string     `gorm:"column:status;size:24;not null;default:'pending';index:idx_wiki_job_claim,priority:2"`
	Stage           string     `gorm:"column:stage;size:32;not null;default:'queued'"`
	ForceCompile    bool       `gorm:"column:force_compile;not null;default:false"`
	SourceIDs       *string    `gorm:"column:source_ids;size:2048;not null;default:''"`
	LeaseOwner      string     `gorm:"column:lease_owner;size:128;not null;default:''"`
	LeaseExpiresAt  *time.Time `gorm:"column:lease_expires_at;index:idx_wiki_job_claim,priority:3"`
	Attempt         uint32     `gorm:"column:attempt;not null;default:0"`
	CancelRequested bool       `gorm:"column:cancel_requested;not null;default:false"`
	ErrorMessage    string     `gorm:"column:error_message;size:2048;not null;default:''"`
	CreatedAt       time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt       *time.Time `gorm:"column:deleted_at;index:idx_wiki_job_deleted"`
}

func (CompileJob) TableName() string { return "wiki_compile_jobs" }
