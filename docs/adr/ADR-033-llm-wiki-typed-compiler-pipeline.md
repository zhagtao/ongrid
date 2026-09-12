# ADR-033：LLM Wiki 使用类型化编译管线

- 状态：已接受
- 日期：2026-09-11
- 关联 RFC：[RFC-004：LLM Wiki 编译流程分层重构](../rfc/RFC-004-llm-wiki-compiler-pipeline.md)
- 关联 HLD：[HLD-002：LLM Wiki 可审计知识编译层](../design/HLD-002-llm-wiki.md)

## 背景

原有 `compileSource` 在一个函数中完成来源读取、文档解析、LLM 语义分析、Topic 规划、知识页面构建、发布和指标记录。各步骤本身已有测试，但阶段数据只以局部变量传递，导致编译主路径较长，副作用边界和可空语义不够直观。

参考 `nashsu/llm_wiki` 的“两阶段分析与生成”思路后，本项目仍需保留更严格的服务端证据校验、事务提交和索引降级语义，不能直接复制其单文件 ingest 实现。

## 决策

在现有 `biz/knowledge/llm_wiki` 包内引入一个类型化、顺序执行的编译管线：

1. `sourceIR`：已解析身份的 Source、SourceVersion 与不可变原文。
2. `parseIR`：规范化 Markdown 与结构化 Chunk。
3. `semanticIR`：Chunk 摘要、来源摘要和 Topic Plan。
4. `knowledgeIR`：Wiki Page、Relation、Evidence 及待发布 manifest。
5. Artifact：写入摘要和 Wiki 文件、提交数据库、更新搜索索引。

管线只负责编排；分块、Prompt、规划、Writer、Reviewer、文件存储和 Repository 继续由现有职责文件实现。所有依赖仍由 `Usecase` 构造函数注入，不新增全局状态或外部依赖。

## 备选方案

### 保持单函数，通过注释分段

改动最小，但阶段契约仍是隐式局部变量，无法阻止解析、语义和发布逻辑再次混合，因此不采用。

### 拆成多个 Go 子包

物理边界最强，但会暴露大量只供编译器内部使用的类型和接口，并可能与 `biz` 内既有消费方接口规则冲突。当前模块规模不需要承担这部分复杂度，因此不采用。

### 引入通用工作流或 DAG 框架

可支持动态编排，但当前流程是固定的五段顺序管线，引入框架会增加依赖、抽象和排障成本，因此不采用。

## 后果

正面影响：主路径可从上到下阅读；每层输入输出明确；持久化副作用集中在 Artifact 阶段；后续增加媒体解析或新的索引实现时无需改写整条流程。

权衡：阶段 IR 是包内额外类型；现有底层方法暂时仍通过 `Usecase` 复用，未来只有在模块继续增长时才考虑下沉为独立组件。

兼容性：不修改 API、数据库 Schema、页面格式、Prompt Schema、缓存键或任务状态语义。
