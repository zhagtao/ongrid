# LLM Wiki Schema v2

本文档是 LLM Wiki 编译器的输出契约说明。实际 Prompt 和 JSON Schema 均以 Go 常量定义；模型无权写文件、写数据库或决定页面路径。

## 1. 编译边界

服务端负责：

- 来源文件读取、PDF/DOCX 文本提取和 Markdown 分块。
- `chunk_id`、正文 rune 区间、来源版本、页面 ID、页面路径、标题、frontmatter、哈希、时间和租户。
- 调用次数、JSON 解码、未知字段、摘要内容和事实证据区间校验。
- 分块摘要文件、层级摘要、页面、来源引用、关系、staging manifest 和索引。
- 确定性生成 `wiki/index.md`，按固定事件 ID 幂等维护 `wiki/log.md`；模型不得生成或覆盖这两个文件。
- 取消、租约、重试、幂等、原子发布和恢复。

编译状态与派生搜索索引统一存放在 `.llm-wiki/wiki.db`。状态表不可由 Markdown 恢复；页面目录、关系、FTS 和向量索引可以从 Wiki 页面重建。

模型只负责理解当前分块文本，并直接返回结构化 JSON 结果。编译器在 Prompt 中提供 JSON Schema，并在服务端严格解码和校验。

## 2. 来源和任务状态

来源状态为：`pending`、`running`、`succeeded`、`stale`、`failed`。

任务状态为：`pending`、`running`、`succeeded`、`skipped`、`failed`、`cancelled`。

原始文件镜像不会调用 LLM。编译任务只由 HTTP compile 接口创建。

## 3. 页面类型和文件格式

服务端只生成两种页面：

- `source`：来源追踪页，保留来源分块摘要、事实与冲突。
- `topic`：面向用户的 canonical Wiki 文章，写入可读 slug 路径；冲突时追加 Topic ID 短后缀，已有 Topic 路径保持不变。

`EntityIR` 和 `ConceptIR` 是知识层语义单元，用于 Topic 规划、归一化、检索和关联展示；它们不再与 Wiki Page 一一对应，也不是 PageType。

页面最大 1 MiB。服务端生成的 frontmatter 字段为：

```yaml
id: <server-generated-page-id>
type: source | topic
title: <server-generated-title>
description: <server-generated-description>
aliases: []
language: und
source_versions: [<server-generated-version-id>]
related: []
created: YYYY-MM-DD
updated: YYYY-MM-DD
```

当前实现不做语言识别，页面 `language` 固定为 `und`。不要猜测或生成其他语言值；源文件语言检测和继承尚未实现。

来源页包含分块摘要、事实和冲突；Topic 页由 Writer 仅根据当前有效 Source Evidence 重写，并通过 `source_versions` 保留溯源关系。

## 4. 分块摘要输出

编译器先将分块原文切成带局部 ID 的 `evidence_spans`。模型只引用这些 ID，不复制原文，也不计算位置：

```json
{
  "input_identifier": "5:0",
  "evidence_spans": [
    {"id": "e0", "text": "第一段原文。"},
    {"id": "e1", "text": "第二段原文。"}
  ]
}
```

必须直接返回且只返回一次如下 JSON 对象：

```json
{
  "summary": "本分块的简洁摘要",
  "entities": [
    {
      "name": "实体名",
      "kind": "实体类型",
      "description": "实体在本分块中的说明"
    }
  ],
  "concepts": [
    {
      "name": "概念名",
      "description": "概念在本分块中的说明"
    }
  ],
  "facts": [
    {
      "statement": "可由当前分块支持的事实",
      "evidence_ids": ["e0"]
    }
  ],
  "conflicts": [
    {
      "statement": "当前分块中的冲突或不一致",
      "with": "冲突对象或相反说法"
    }
  ]
}
```

要求：

1. 只返回一个 JSON 对象，不要输出 Markdown fenced code、解释文字或其他内容。
2. `evidence_ids` 必须存在于当前分块，且多个 ID 必须唯一、按原文顺序连续。服务端将其还原为可信的原文 rune 区间。
2. 只能使用上述字段；未知字段会被拒绝。
3. `summary` 非空；没有实体、概念、事实或冲突时使用空数组。
4. `entities` 的每项必须有 `name`、`kind`、`description`。
5. `concepts` 的每项必须有 `name`、`description`。
6. `facts.evidence` 必须是当前分块中的连续原文。服务端负责定位原文并生成最终 IR 的 rune 起止偏移；无法定位的 fact 会被丢弃。
7. 不要引用分块之外的内容；不确定的内容不要写成事实。
8. 不要输出路径、文件名、哈希、时间戳、数据库 ID 或自定义关系。

## 5. 层级摘要

当一个来源有多个叶子分块时，服务端会把已有摘要组合成输入并请求层级摘要。每个来源版本的最终摘要会持久化到 `.llm-wiki/summaries/<version>.json`，供后续文件复用。层级摘要仍使用完全相同的 JSON 结构，但 `facts` 会被服务端置为空数组；层级摘要最多执行 8 层，并要求结果收敛。只有最后的服务端生成器可以将摘要转换为页面。

层级摘要必须合并重复的实体和概念，保留重要技术细节、限制和跨分块关系；不能引入叶子摘要中不存在的新事实。叶子摘要之间存在分歧时，应记录到 `conflicts`，不能静默选择一方。`Summary` 编号、JSON 键和分块顺序不是知识内容。

层级摘要只作为 Topic Planner 的压缩输入，不直接生成派生页面，也不作为后续重写的事实来源。

## 6. 页面和关系生成

Topic Planner 根据叶子摘要和层级摘要产生少量 `TopicPlan`，Canonical Resolver 将 proposal 映射到持久化 Topic。TopicID/PageID 是不可变身份；标题和别名更新不会改变已有页面路径。服务端把来源页到 Topic 页的关系写入 frontmatter `related` 和正文 `[[target|label]]`。应用从 Markdown 按相对路径、标题、alias 解析关系，去重并忽略自环；断链只产生结构化告警。

Topic Evidence 显式记录 `topic_id`、`source_id`、`source_version_id` 和 chunk/rune 范围。同一 Source 的新版本替换旧 Evidence；不同 Source 的 Evidence 同时保留，冲突交给 Writer 显式表达。Markdown 是页面与关系事实源；事实内容重写仍必须回到 SourceVersion 与 TopicEvidence。

关系校验白名单为：

```text
mentions
depends_on
related_to
conflicts_with
supersedes
```

当前编译流程实际只产生 `mentions`；模型不能直接提交上述关系。

## 7. 编译流程

```text
raw/version
  -> extract
  -> SplitMarkdown
  -> leaf summary IR + validate
  -> .llm-wiki/chunks/<version>/<ordinal>.json
  -> hierarchy summary
  -> .llm-wiki/summaries/<version>.json
  -> Topic Planner + strict validation
  -> Canonical Resolver
  -> effective Evidence merge
  -> promotion gate
  -> evidence-backed Writer
  -> deterministic quality review
  -> source/topic pages
  -> generation validation
  -> .llm-wiki/staging/<job>/manifest.json
  -> Topic/Evidence/Page DB transaction
  -> atomic file publish recovery
  -> optional index update
```

每个分块只调用一次 LLM。Planner 允许一次严格 JSON repair；Writer 的每个 section 必须引用输入 Evidence。任何必需 Topic 的 Writer 失败都会终止本次 Source 编译，Topic/Evidence/Page 数据不会部分提交。生成指纹未变化时复用已有 Topic 正文并跳过重复索引。

## 8. 可用工具

Agent 对外只通过 `query_knowledge` 按 `hybrid`、`wiki` 或 `rag` 查询 Wiki。

分块摘要 JSON 仅由编译器内部的 `LLMSummarizer` 解析，不是 Agent 的公共接口。
