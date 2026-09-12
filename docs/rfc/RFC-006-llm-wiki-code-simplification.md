# RFC-006：LLM Wiki 编译代码简化

## 元信息

- 状态：草稿（实现已完成，待评审）
- 日期：2026-09-12
- 关联 RFC：[RFC-004：LLM Wiki 编译流程分层重构](RFC-004-llm-wiki-compiler-pipeline.md)
- 范围：`internal/manager/biz/knowledge/llm_wiki`

## 背景

RFC-004 建立了 Source → Parse → Semantic → Knowledge → Artifact 的编译管线，但 chunk 编译、LLM 响应校验和 Topic Planner 仍有重复分支。重复逻辑增加了阅读成本，也让缓存、批处理和错误处理的行为难以集中核对。

## 方案

- 使用一个严格 JSON 解码器统一 unknown fields 与尾部内容校验，供 Compiler、Planner、Writer 复用。
- Source Synthesis 直接复用已解码的摘要和 Topic 列表，移除重复解码及 JSON marshal/unmarshal 往返。
- 将 `compileChunks` 拆为缓存读取、缺失 chunk 编译、结果合并、摘要持久化四步；批量预算只计算一次。
- 统一各 LLM 阶段的 token 使用记录，并保留原有错误上下文和调用顺序。
- 预计算 Planner 所需的 chunk token 数和 rune 长度，避免对同一 chunk 重复扫描。

## 备选方案

### 只增加注释

改动最小，但重复的 JSON 校验、缓存分支和 Planner 输入构造仍然存在，无法降低维护成本。

### 拆成多个子包

物理隔离更强，但会暴露仅供管线内部使用的类型，增加依赖边界和迁移成本；当前包内私有辅助函数已足够表达阶段边界。

## 影响范围

- 代码：仅 LLM Wiki 业务层编译、规划和写作辅助逻辑。
- API/数据：无 API、Proto、数据库 Schema、文件格式或缓存协议变更。
- 运行时：LLM 调用顺序、缓存命中语义、错误处理、Token 统计和发布顺序保持不变；缓存校验减少重复计算。
- 回滚：回退本 RFC 涉及的代码和文档即可，不需要数据迁移；已有 Wiki 文件和 SQLite 数据不受影响。

## 风险与缓解

- 严格解码器改变错误文本：保留 `errs.ErrInvalid` 和 `%w` 错误链，并由现有校验测试覆盖。
- 拆分 chunk 编译时改变阶段顺序：主流程按“缓存 → 编译 → 合并 → 持久化”固定编排，并执行包级竞态测试。
- Planner 预计算长度与原逻辑不一致：使用同一 `estimateTokens` 和 `utf8.RuneCountInString` 实现，现有 Topic 校验测试覆盖边界。

## 实施任务

- [x] 收敛严格 JSON 解码和 Token Usage 记录逻辑。
- [x] 拆分 chunk 编译流程并移除重复扫描/JSON 往返。
- [x] 运行 LLM Wiki 及相关 Manager 包的 `-race` 测试和 `go vet`。
- [ ] 完成至少 3 个工作日评审后更新 RFC 状态。

## 验收标准

- `compileChunks` 主流程只表达四个编译步骤，缓存和批处理错误分支不再嵌套在主流程中。
- Source Synthesis 不再重复解析 source summary 或通过 JSON 往返校验 topics。
- `go test -race ./internal/manager/biz/knowledge/llm_wiki` 通过。
- 相关 Knowledge/Data/Service/Server 包的竞态测试和 `go vet` 通过。
