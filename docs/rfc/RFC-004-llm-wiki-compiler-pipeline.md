# RFC-004：LLM Wiki 编译流程分层重构

## 元信息

- 状态：已完成
- 日期：2026-09-11
- 关联 ADR：[ADR-033：LLM Wiki 使用类型化编译管线](../adr/ADR-033-llm-wiki-typed-compiler-pipeline.md)
- 关联 HLD：[HLD-002：LLM Wiki 可审计知识编译层](../design/HLD-002-llm-wiki.md)

## 背景

LLM Wiki 已具备分块、缓存、来源综合、Topic 规划、证据写作、质量门禁、原子发布和混合索引能力，但核心编译路径集中在 `compileSource`，阶段边界依赖阅读实现才能理解。此次重构的目标是保持行为和外部契约不变，把编译过程表达为清晰的五层数据流。

## 方案

编译流程固定为：

```text
Source Layer → Parse IR → Semantic IR → Knowledge IR → Artifact Layer
```

| 阶段 | 输入 | 输出 | 主要职责 |
| --- | --- | --- | --- |
| Source | job、source ID | `sourceIR` | 加载 Source/Version、跳过未变化版本、读取快照 |
| Parse | `sourceIR` | `parseIR` | 提取 Markdown、结构化分块、保留来源元数据 |
| Semantic | `parseIR` | `semanticIR` | Chunk 分析与缓存、来源摘要、Topic 规划 |
| Knowledge | `semanticIR` | `knowledgeIR` | canonical Topic、Evidence、Wiki Page、Relation、质量门禁 |
| Artifact | `knowledgeIR` | IndexDocument | 摘要/页面发布、数据库提交、搜索索引与指标 |

阶段 IR 保持包内私有，避免形成新的公共 API。可选 Source Synthesis 失败时继续回退到层级摘要与 Planner；索引失败仍只把 job stage 标记为 `index_failed`，不回滚已发布页面。

## 备选方案

### 仅拆函数，不定义 IR

代码行数会下降，但参数列表继续增长，阶段边界无法由类型表达。

### 每层独立子包

隔离更强，但需要公开内部模型并重组大量现有方法，迁移成本和循环依赖风险高。

### 工作流框架

适合动态 DAG；本流程固定且强调可审计顺序，框架收益不足以覆盖复杂度。

## 影响范围

- 代码：仅 `internal/manager/biz/knowledge/llm_wiki` 编译编排及其测试。
- API/数据：无变更，无 migration。
- 运行时：LLM 调用顺序、缓存、发布事务、索引降级和 Token 统计保持不变。
- 迁移：无需数据迁移。
- 回滚：回退 `pipeline.go`，恢复 `compileSource` 原内联实现即可；现有数据和文件产物不需处理。

## 风险与缓解

- 可空来源摘要语义被误收紧：分别保留可空 `fileSummary` 与必有的持久化 `sourceSummary`，并以集成测试覆盖。
- 发布顺序变化：Artifact 阶段维持“先写摘要，再发布 manifest/页面”的顺序。
- 索引失败误伤编译：索引仍在发布完成后执行，失败只产生 `index_failed`。

## 实施任务

- [x] 建立五段 IR 与管线编排。
- [x] 将搜索索引收敛到 Artifact 辅助方法。
- [x] 增加 Parse IR 和索引行为单元测试。
- [x] 完成包级与相关 Manager 测试、自查并更新状态。

## 验收标准

- `compileSource` 只保留管线入口，不再混合阶段实现。
- 每个阶段有明确、私有的输入输出类型。
- API、数据库和生成页面契约无变化。
- `go test -race ./internal/manager/biz/knowledge/llm_wiki` 通过。
- 相关 Manager 包测试通过。

## 排期

- 2026-09-11：分析、实现、单元测试和竞态验证。
- 2026-09-12：评审与合并。

## 变更记录

| 日期 | 变更 | 原因 |
| --- | --- | --- |
| 2026-09-11 | 初版 | 建立 LLM Wiki 五层编译管线 |
