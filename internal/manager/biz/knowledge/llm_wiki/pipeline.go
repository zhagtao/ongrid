package llm_wiki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/docextract"
	"github.com/ongridio/ongrid/internal/pkg/llm"
)

// 编译器使用类型化 IR 串联固定阶段。每一层只消费上一层输出，避免解析、
// 语义分析、知识生成和发布逻辑互相渗透。
type sourceIR struct {
	source  *model.Source
	version *model.SourceVersion
	body    []byte
}

type parseIR struct {
	source   *model.Source
	version  *model.SourceVersion
	markdown string
	chunks   []Chunk
}

type semanticIR struct {
	source         *model.Source
	version        *model.SourceVersion
	chunks         []Chunk
	chunkSummaries []ChunkSummaryIR
	fileSummary    *ChunkSummaryIR
	sourceSummary  *ChunkSummaryIR
	topicPlans     []TopicPlan
}

type knowledgeIR struct {
	versionID   uint64
	summaryJSON []byte
	compilation sourceCompilation
}

type compilePipeline struct {
	usecase *Usecase
}

type compileStatsUsageSink struct {
	inner        TokenUsageSink
	inputTokens  int64
	outputTokens int64
}

func (s *compileStatsUsageSink) Record(ctx context.Context, usage llm.Usage) error {
	if err := s.inner.Record(ctx, usage); err != nil {
		return err
	}
	s.inputTokens += int64(usage.PromptTokens)
	s.outputTokens += int64(usage.CompletionTokens)
	return nil
}

func (s *compileStatsUsageSink) Close(context.Context) error { return nil }

func newCompilePipeline(usecase *Usecase) *compilePipeline {
	return &compilePipeline{usecase: usecase}
}

// compile 按 Source → Parse → Semantic → Knowledge → Artifact 的固定顺序执行。
func (p *compilePipeline) compile(ctx context.Context, job *model.CompileJob, sourceID uint64, usageSink TokenUsageSink) ([]IndexDocument, error) {
	usage := &compileStatsUsageSink{inner: usageSink}

	source, skipped, err := p.loadSource(ctx, job, sourceID)
	if err != nil || skipped {
		return nil, err
	}
	parsed, err := parseSource(source)
	if err != nil {
		return nil, err
	}
	semantic, err := p.analyze(ctx, job, parsed, usage)
	if err != nil {
		return nil, err
	}
	knowledge, err := p.buildKnowledge(ctx, semantic, usage)
	if err != nil {
		return nil, err
	}
	if err := p.publish(ctx, job, sourceID, knowledge); err != nil {
		return nil, err
	}

	stats := &knowledge.compilation.stats
	stats.InputTokens = usage.inputTokens
	stats.OutputTokens = usage.outputTokens
	observeCompileStats(*stats)
	p.usecase.log.InfoContext(ctx, "wiki source compiled",
		slog.Uint64("source_id", sourceID),
		slog.Int("chunks", stats.ChunkCount),
		slog.Int("candidate_pages", stats.CandidatePageCount),
		slog.Int("published_pages", stats.PublishedPageCount),
		slog.Int("dropped_pages", stats.DroppedPageCount),
		slog.Int64("llm_input_tokens", stats.InputTokens),
		slog.Int64("llm_output_tokens", stats.OutputTokens),
	)
	return knowledge.compilation.documents, nil
}

// loadSource 实现 Source Layer：解析来源身份并读取不可变版本快照。
func (p *compilePipeline) loadSource(ctx context.Context, job *model.CompileJob, sourceID uint64) (*sourceIR, bool, error) {
	if job == nil {
		return nil, false, errors.New("llmwiki: compile job is required")
	}
	source, err := p.usecase.repo.GetSource(ctx, job.TenantID, sourceID)
	if err != nil {
		return nil, false, err
	}
	if source.CurrentVersionID == nil {
		return nil, false, errors.New("llmwiki: source has no version")
	}
	version, err := p.usecase.repo.GetVersion(ctx, job.TenantID, *source.CurrentVersionID)
	if err != nil {
		return nil, false, err
	}
	if source.Status == model.SourceSucceeded && version.SchemaVersion == SchemaVersion && !job.ForceCompile {
		return nil, true, nil
	}
	if err := p.usecase.repo.MarkSourceStatus(ctx, job.TenantID, sourceID, model.SourceRunning); err != nil {
		return nil, false, err
	}
	body, err := p.usecase.files.Read(ctx, version.SnapshotPath, MaxSourceBytes)
	if err != nil {
		return nil, false, err
	}
	return &sourceIR{source: source, version: version, body: body}, false, nil
}

// parseSource 实现 Parse Layer：把来源统一为 Markdown，并生成稳定 Chunk。
func parseSource(input *sourceIR) (*parseIR, error) {
	if input == nil || input.source == nil || input.version == nil {
		return nil, errors.New("llmwiki: source IR is incomplete")
	}
	markdown, err := docextract.Extract(input.source.RawPath, input.body)
	if err != nil {
		return nil, fmt.Errorf("llmwiki: extract source %q: %w", input.source.RawPath, err)
	}
	chunks := SplitMarkdown(markdown)
	if len(chunks) == 0 {
		return nil, errors.New("llmwiki: source is empty")
	}
	return &parseIR{source: input.source, version: input.version, markdown: markdown, chunks: chunks}, nil
}

// analyze 实现 Semantic Layer：生成受证据约束的摘要与 Topic Plan。
func (p *compilePipeline) analyze(ctx context.Context, job *model.CompileJob, parsed *parseIR, usage TokenUsageSink) (*semanticIR, error) {
	summaries, _, sourceSummary, err := p.usecase.compileChunks(ctx, job, parsed.version.ID, parsed.chunks, usage)
	if err != nil {
		return nil, err
	}
	if err := p.usecase.ensureCompileActive(ctx, job); err != nil {
		return nil, err
	}

	var plans []TopicPlan
	if sourceSummary != nil && p.usecase.plannerConfig().EnableSourceSynthesis {
		synthesizedSummary, synthesizedPlans, synthErr := p.usecase.synthesizeSource(ctx, parsed.source, parsed.version, parsed.chunks, summaries, sourceSummary, usage)
		if synthErr == nil {
			sourceSummary = synthesizedSummary
			plans = synthesizedPlans
		} else if errors.Is(synthErr, context.Canceled) || errors.Is(synthErr, context.DeadlineExceeded) {
			return nil, synthErr
		} else {
			p.usecase.log.WarnContext(ctx, "wiki source synthesis fallback", slog.Uint64("version_id", parsed.version.ID), slog.Any("err", synthErr))
		}
	}
	if sourceSummary == nil {
		sourceSummary, err = p.usecase.summarizeHierarchy(ctx, parsed.version.ID, summaries, usage)
		if err != nil {
			return nil, err
		}
	}
	if plans == nil {
		plans, err = p.usecase.planTopics(ctx, parsed.source, parsed.version, parsed.chunks, summaries, sourceSummary, usage)
		if err != nil {
			return nil, err
		}
	}
	persistedSummary := &summaries[0]
	if sourceSummary != nil {
		persistedSummary = sourceSummary
	}
	return &semanticIR{
		source:         parsed.source,
		version:        parsed.version,
		chunks:         parsed.chunks,
		chunkSummaries: summaries,
		fileSummary:    sourceSummary,
		sourceSummary:  persistedSummary,
		topicPlans:     plans,
	}, nil
}

// buildKnowledge 实现 Knowledge Layer：生成并校验页面、关系与证据集合。
func (p *compilePipeline) buildKnowledge(ctx context.Context, semantic *semanticIR, usage TokenUsageSink) (*knowledgeIR, error) {
	if semantic == nil || len(semantic.chunkSummaries) == 0 || semantic.sourceSummary == nil {
		return nil, errors.New("llmwiki: semantic IR is incomplete")
	}
	summaryJSON, err := json.Marshal(semantic.sourceSummary)
	if err != nil {
		return nil, fmt.Errorf("llmwiki: encode source summary: %w", err)
	}
	compilation, err := p.usecase.buildSourceCompilation(
		ctx, semantic.source, semantic.version, semantic.chunks,
		semantic.chunkSummaries, semantic.fileSummary, semantic.topicPlans, usage,
	)
	if err != nil {
		return nil, err
	}
	if err := finalizeSourceCompilation(compilation); err != nil {
		return nil, err
	}
	return &knowledgeIR{versionID: semantic.version.ID, summaryJSON: summaryJSON, compilation: compilation}, nil
}

// publish 实现 Artifact Layer：先保存摘要，再原子发布 Wiki 产物。
func (p *compilePipeline) publish(ctx context.Context, job *model.CompileJob, sourceID uint64, knowledge *knowledgeIR) error {
	if knowledge == nil {
		return errors.New("llmwiki: knowledge IR is required")
	}
	if err := p.usecase.files.WriteSourceSummary(ctx, knowledge.versionID, knowledge.summaryJSON); err != nil {
		return err
	}
	return p.usecase.publishSourceCompilation(ctx, job, sourceID, knowledge.versionID, knowledge.compilation)
}

// indexArtifacts 在发布完成后更新可重建的搜索索引，索引失败不回滚 Wiki。
func (u *Usecase) indexArtifacts(ctx context.Context, documents []IndexDocument) bool {
	if u.indexer == nil {
		return false
	}
	failed := false
	for _, document := range documents {
		if document.SkipIndex {
			continue
		}
		if err := u.indexer.IndexPage(ctx, document); err != nil {
			failed = true
			u.log.WarnContext(ctx, "wiki page index failed", slog.String("page_id", document.Page.PageID), slog.Any("err", err))
		}
	}
	return failed
}
