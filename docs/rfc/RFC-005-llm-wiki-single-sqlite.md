# RFC-005：LLM Wiki 全文件化与单 SQLite

- 状态：已实施
- 日期：2026-09-12
- 范围：`internal/manager/{biz,data}/knowledge/llm_wiki`、`internal/manager/service/knowledge/llmwiki_usage.go`、`internal/manager/server/knowledge/llmwiki_http.go`、Manager 组合根、数据库迁移

## 背景

LLM Wiki 的页面位于本地文件系统，但编译状态、页面目录和关系曾写入全局 MySQL，向量写入 Qdrant。这让一个本地 Wiki 同时依赖三个持久化系统，页面关系也无法在 Markdown 中审阅或携带。

## 决策

每个 Wiki 根目录只拥有一个 `.llm-wiki/wiki.db`。`sources`、`source_versions`、`source_chunks`、`compile_jobs`、`topics`、`topic_evidence` 是不可丢状态；`wiki_pages`、`wiki_page_sources`、`wiki_links`、`wiki_fts`、`wiki_vectors`、`wiki_index_meta` 是可从 Markdown 重建的查询模型。

页面与关系以 `wiki/**/*.md` 为事实源。frontmatter 的 `related` 与正文 `[[wikilink]]` 统一解析，按路径、标题、alias 定位，去重、自环忽略、断链告警。应用确定性维护 `wiki/index.md`，按 event ID 幂等维护逆序 `wiki/log.md`；这两个文件不进入目录或搜索索引。

搜索使用 SQLite FTS5 trigram 和页面级 float32 BLOB，进程内计算余弦相似度，继续通过 RRF 融合。embedding 不可用时保留 FTS；embedding 失败会移除对应旧向量并将任务阶段记为 `index_failed`。旧 RAG 的 Qdrant 使用不受影响。

## 编译与恢复

编译保持 Source → Parse IR → Semantic IR → Knowledge IR → Artifact 五层。Artifact 层将候选页面写入 `.llm-wiki/staging/<job>/wiki`，保存含固定事件 ID/UTC 时间的 manifest，提交 SQLite 状态事务后再原子激活文件。提交后激活失败会保留 manifest；Manager 在注册 HTTP 服务前 reconcile 并完成激活、目录/日志刷新和派生索引重建。

派生索引重建只清空当前租户的 `wiki_pages`、`wiki_page_sources`、`wiki_links` 和 FTS 内容，并清理孤立向量，不删除或替换 `wiki.db`。数据库整体损坏时启动失败，必须从备份恢复。

## 迁移与回滚

全局数据库启动迁移不再注册 LLM Wiki GORM migration。MySQL cleanup migration 删除全部 `knowledge_wiki_*` 表，不做复制或双写；down 仅恢复空表。回滚时保留 `wiki.db` 和版本文件，从 Raw/Version 重新编译。

## 影响

- 优点：Wiki 可携带、关系可审阅，单一备份边界，移除 LLM Wiki 的 MySQL/Qdrant 运行依赖。
- 代价：SQLite 依赖单实例与持久本地卷；向量检索为进程内线性扫描，规模扩大后需重新评估。
- 安全：Wiki 目录为 `0750`，数据库与产物为 `0640`；SQLite 使用 WAL、foreign keys 和 5 秒 busy timeout。
