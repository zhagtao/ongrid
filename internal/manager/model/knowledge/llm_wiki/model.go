// Package llm_wiki contains persistence entities for the LLM Wiki bounded context.
package llm_wiki

import "time"

const (
	PageTypeSource = "source"
	PageTypeTopic  = "topic"

	TopicPending   = "pending"
	TopicPublished = "published"

	SourcePending   = "pending"
	SourceRunning   = "running"
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
	ActiveJobID      *uint64    `gorm:"column:active_job_id;index:idx_wiki_source_active_job"`
	ContentSHA256    string     `gorm:"column:content_sha256;size:64;not null;default:''"`
	Status           string     `gorm:"column:status;size:24;not null;default:'pending';index:idx_wiki_source_status,priority:2"`
	CreatedAt        time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt        time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt        *time.Time `gorm:"column:deleted_at;index:idx_wiki_source_deleted"`
}

func (Source) TableName() string { return "sources" }

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

func (SourceVersion) TableName() string { return "source_versions" }

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

func (SourceChunk) TableName() string { return "source_chunks" }

type CompileJob struct {
	ID              uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID        uint64     `gorm:"column:tenant_id;not null;default:0;index:idx_wiki_job_claim,priority:1"`
	SourceIDsJSON   string     `gorm:"column:source_ids_json;type:json;not null"`
	ActiveKey       *string    `gorm:"column:active_key;size:64;uniqueIndex:uk_wiki_job_active"`
	Status          string     `gorm:"column:status;size:24;not null;default:'pending';index:idx_wiki_job_claim,priority:2"`
	Stage           string     `gorm:"column:stage;size:32;not null;default:'queued'"`
	ForceCompile    bool       `gorm:"column:force_compile;not null;default:false"`
	LeaseOwner      string     `gorm:"column:lease_owner;size:128;not null;default:''"`
	LeaseExpiresAt  *time.Time `gorm:"column:lease_expires_at;index:idx_wiki_job_claim,priority:3"`
	Attempt         uint32     `gorm:"column:attempt;not null;default:0"`
	CancelRequested bool       `gorm:"column:cancel_requested;not null;default:false"`
	ErrorMessage    string     `gorm:"column:error_message;size:2048;not null;default:''"`
	CreatedAt       time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt       *time.Time `gorm:"column:deleted_at;index:idx_wiki_job_deleted"`
}

func (CompileJob) TableName() string { return "compile_jobs" }

type Page struct {
	ID                 uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	PageID             string     `gorm:"column:page_id;size:64;not null;default:'';uniqueIndex:uk_wiki_page_id"`
	TenantID           uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_page_path,priority:1;index:idx_wiki_page_type,priority:1"`
	PageType           string     `gorm:"column:page_type;size:24;not null;default:'source';index:idx_wiki_page_type,priority:2"`
	Title              string     `gorm:"column:title;size:512;not null;default:''"`
	AliasesJSON        string     `gorm:"column:aliases_json;type:json;not null"`
	Language           string     `gorm:"column:language;size:16;not null;default:''"`
	RelativePath       string     `gorm:"column:relative_path;size:1024;not null;default:''"`
	RelativePathSHA256 *string    `gorm:"column:relative_path_sha256;size:64;uniqueIndex:uk_wiki_page_path,priority:2"`
	BodySHA256         string     `gorm:"column:body_sha256;size:64;not null;default:''"`
	CreatedAt          time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt          time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt          *time.Time `gorm:"column:deleted_at;index:idx_wiki_page_deleted"`
}

func (Page) TableName() string { return "wiki_pages" }

// Topic is the durable canonical identity behind one generated topic page.
// TopicID remains stable when the title or aliases change; Page is only the
// current materialized view of the effective evidence attached to the topic.
type Topic struct {
	ID                    uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TopicID               string     `gorm:"column:topic_id;size:64;not null;default:'';uniqueIndex:uk_wiki_topic_id"`
	TenantID              uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_topic_key,priority:1;index:idx_wiki_topic_status,priority:1"`
	CanonicalKey          string     `gorm:"column:canonical_key;size:512;not null;default:'';uniqueIndex:uk_wiki_topic_key,priority:2"`
	Title                 string     `gorm:"column:title;size:512;not null;default:''"`
	AliasesJSON           string     `gorm:"column:aliases_json;type:json;not null"`
	EntityNamesJSON       string     `gorm:"column:entity_names_json;type:json;not null"`
	ConceptNamesJSON      string     `gorm:"column:concept_names_json;type:json;not null"`
	Summary               string     `gorm:"column:summary;type:text;not null"`
	GenerationFingerprint string     `gorm:"column:generation_fingerprint;size:64;not null;default:''"`
	Status                string     `gorm:"column:status;size:24;not null;default:'pending';index:idx_wiki_topic_status,priority:2"`
	CreatedAt             time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt             time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt             *time.Time `gorm:"column:deleted_at;index:idx_wiki_topic_deleted"`
}

func (Topic) TableName() string { return "topics" }

// TopicEvidence records source-grounded provenance. The referenced raw text
// is loaded from SourceVersion + ChunkOrdinal so derived Wiki Markdown never
// becomes the factual source for a later rewrite.
type TopicEvidence struct {
	ID              uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID        uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_topic_evidence,priority:1;index:idx_wiki_evidence_source,priority:1"`
	TopicID         string     `gorm:"column:topic_id;size:64;not null;default:'';uniqueIndex:uk_wiki_topic_evidence,priority:2;index:idx_wiki_evidence_topic"`
	SourceID        uint64     `gorm:"column:source_id;not null;uniqueIndex:uk_wiki_topic_evidence,priority:3;index:idx_wiki_evidence_source,priority:2"`
	SourceVersionID uint64     `gorm:"column:source_version_id;not null;uniqueIndex:uk_wiki_topic_evidence,priority:4"`
	ChunkOrdinal    uint32     `gorm:"column:chunk_ordinal;not null;default:0;uniqueIndex:uk_wiki_topic_evidence,priority:5"`
	EvidenceStart   uint64     `gorm:"column:evidence_start;not null;default:0;uniqueIndex:uk_wiki_topic_evidence,priority:6"`
	EvidenceEnd     uint64     `gorm:"column:evidence_end;not null;default:0;uniqueIndex:uk_wiki_topic_evidence,priority:7"`
	Kind            string     `gorm:"column:kind;size:24;not null;default:'context'"`
	CreatedAt       time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt       *time.Time `gorm:"column:deleted_at;index:idx_wiki_evidence_deleted"`
}

func (TopicEvidence) TableName() string { return "topic_evidence" }

type PageSource struct {
	ID              uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID        uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_page_source,priority:1;index:idx_wiki_page_source_version,priority:1"`
	PageID          string     `gorm:"column:page_id;size:64;not null;default:'';uniqueIndex:uk_wiki_page_source,priority:2"`
	SourceVersionID uint64     `gorm:"column:source_version_id;not null;uniqueIndex:uk_wiki_page_source,priority:3;index:idx_wiki_page_source_version,priority:2"`
	ChunkID         *uint64    `gorm:"column:chunk_id;uniqueIndex:uk_wiki_page_source,priority:4"`
	CreatedAt       time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt       time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt       *time.Time `gorm:"column:deleted_at;index:idx_wiki_page_source_deleted"`
}

func (PageSource) TableName() string { return "wiki_page_sources" }

type PageRelation struct {
	ID           uint64     `gorm:"column:id;primaryKey;autoIncrement"`
	TenantID     uint64     `gorm:"column:tenant_id;not null;default:0;uniqueIndex:uk_wiki_relation,priority:1;index:idx_wiki_relation_to,priority:1"`
	FromPageID   string     `gorm:"column:from_page_id;size:64;not null;default:'';uniqueIndex:uk_wiki_relation,priority:2"`
	ToPageID     string     `gorm:"column:to_page_id;size:64;not null;default:'';uniqueIndex:uk_wiki_relation,priority:3;index:idx_wiki_relation_to,priority:2"`
	RelationType string     `gorm:"column:relation_type;size:64;not null;default:'';uniqueIndex:uk_wiki_relation,priority:4"`
	CreatedAt    time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt    time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	DeletedAt    *time.Time `gorm:"column:deleted_at;index:idx_wiki_relation_deleted"`
}

func (PageRelation) TableName() string { return "wiki_links" }
