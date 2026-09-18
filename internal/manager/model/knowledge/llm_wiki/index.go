package llm_wiki

// WikiIndexMeta stores search index metadata such as the schema version.
type WikiIndexMeta struct {
	Key   string `gorm:"column:key;size:64;primaryKey"`
	Value string `gorm:"column:value;size:255;not null"`
}

func (WikiIndexMeta) TableName() string { return "wiki_index_meta" }

// WikiLexical stores the portable lexical search row for one page. It is a
// regular table with LIKE-based substring matching so SQLite and MySQL share
// the same query semantics. Rebuildable from wiki_build_pages and artifacts.
type WikiLexical struct {
	PageKey  string `gorm:"column:page_key;size:96;primaryKey"`
	TenantID uint64 `gorm:"column:tenant_id;not null;index:idx_wiki_lexical_tenant"`
	PageID   string `gorm:"column:page_id;size:64;not null"`
	PageType string `gorm:"column:page_type;size:24;not null;default:''"`
	Title    string `gorm:"column:title;size:512;not null;default:''"`
	Aliases  string `gorm:"column:aliases;size:512;not null;default:''"`
	Content  string `gorm:"column:content;type:longtext;not null"`
}

func (WikiLexical) TableName() string { return "wiki_lexical" }
