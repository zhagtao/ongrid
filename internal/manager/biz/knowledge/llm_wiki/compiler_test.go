package llm_wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	model "github.com/ongridio/ongrid/internal/manager/model/knowledge/llm_wiki"
	"github.com/ongridio/ongrid/internal/pkg/llm"
)

type compileChunkRepoStub struct{ Repository }

func (compileChunkRepoStub) IsCancelRequested(context.Context, uint64, uint64) (bool, error) {
	return false, nil
}

func (compileChunkRepoStub) ReplaceChunks(context.Context, uint64, uint64, []*model.SourceChunk) error {
	return nil
}

type batchCompilerStub struct {
	batchCalls  int
	singleCalls int
}

func (s *batchCompilerStub) Summarize(context.Context, string, string, string) (string, error) {
	s.singleCalls++
	return `{"summary":"single","entities":[],"concepts":[],"facts":[],"conflicts":[]}`, nil
}

func (s *batchCompilerStub) SummarizeBatch(_ context.Context, _ string, chunks []BatchChunkInput, _ int) (string, llm.Usage, error) {
	s.batchCalls++
	items := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		text := ""
		for _, span := range chunk.EvidenceSpans {
			text += span.Text
		}
		items = append(items, fmt.Sprintf(`{"chunk_id":%q,"summary":{"summary":"%s","entities":[],"concepts":[],"facts":[],"conflicts":[]}}`, chunk.ChunkID, text))
	}
	return fmt.Sprintf(`{"source_summary":null,"chunks":[%s]}`, strings.Join(items, ",")), llm.Usage{}, nil
}

type invalidBatchCompilerStub struct {
	batchCalls  int
	singleCalls int
}

func (s *invalidBatchCompilerStub) Summarize(context.Context, string, string, string) (string, error) {
	s.singleCalls++
	return `{"summary":"single","entities":[],"concepts":[],"facts":[],"conflicts":[]}`, nil
}

func (s *invalidBatchCompilerStub) SummarizeBatch(context.Context, string, []BatchChunkInput, int) (string, llm.Usage, error) {
	s.batchCalls++
	return `{"source_summary":null,"chunks":[{"chunk_id":"7:0","summary":{"summary":"one","entities":[{"name":"service","kind":"service","description":"service","role":"extra"}],"concepts":[],"facts":[],"conflicts":[]}}]}`, llm.Usage{}, nil
}

type compilerClientStub struct{ response llm.Message }

func (s compilerClientStub) Chat(context.Context, llm.ChatReq) (*llm.ChatResp, error) {
	return &llm.ChatResp{Assistant: s.response}, nil
}

type capturingCompilerClient struct {
	request  llm.ChatReq
	response llm.Message
}

func (c *capturingCompilerClient) Chat(_ context.Context, request llm.ChatReq) (*llm.ChatResp, error) {
	c.request = request
	return &llm.ChatResp{Assistant: c.response}, nil
}

func TestLLMSummarizer_ReturnsDirectJSON(t *testing.T) {
	valid := `{"summary":"ok","entities":[],"concepts":[],"facts":[],"conflicts":[]}`
	summarizer := NewLLMSummarizer(compilerClientStub{response: llm.Message{Content: valid}})
	got, err := summarizer.Summarize(context.Background(), leafInstructions(), "1:0", "body")
	if err != nil {
		t.Fatal(err)
	}
	if got != valid {
		t.Fatalf("summary = %s", got)
	}

	summarizer = NewLLMSummarizer(compilerClientStub{response: llm.Message{ToolCalls: []llm.ToolCall{{Name: "fake", Args: json.RawMessage(valid)}}}})
	if _, err := summarizer.Summarize(context.Background(), leafInstructions(), "1:0", "body"); err == nil {
		t.Fatal("tool calls must not be accepted")
	}
}

func TestValidateHierarchySummary_DropsFacts(t *testing.T) {
	raw := `{"summary":"aggregate","entities":[],"concepts":[],"facts":[{"statement":"fact","evidence_ids":["e0"]}],"conflicts":[]}`
	got, err := ValidateHierarchySummary(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 0 {
		t.Fatalf("hierarchy facts = %+v, want facts omitted", got.Facts)
	}
}

func TestValidateChunkSummary_RejectsUnknownEvidenceIDs(t *testing.T) {
	raw := `{"summary":"ok","entities":[],"concepts":[],"facts":[{"statement":"valid","evidence_ids":["e0"]},{"statement":"invalid","evidence_ids":["missing"]}],"conflicts":[]}`
	if _, err := ValidateChunkSummary(raw, "前言：知识段落。结尾"); err == nil {
		t.Fatal("accepted a fact with an unknown evidence id")
	}
}

func TestValidateChunkSummary_ResolvesContiguousEvidenceIDsToOriginalRunes(t *testing.T) {
	source := strings.Repeat("甲", minimumEvidenceSpanRunes) + "。" + strings.Repeat("乙", minimumEvidenceSpanRunes) + "。"
	spans := buildEvidenceSpans(source)
	if len(spans) != 2 {
		t.Fatalf("evidence spans = %+v, want two", spans)
	}
	raw := `{"summary":"ok","entities":[],"concepts":[],"facts":[{"statement":"跨片段事实","evidence_ids":["e0","e1"]}],"conflicts":[]}`
	got, err := ValidateChunkSummary(raw, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Facts) != 1 || got.Facts[0].EvidenceStart != 0 || got.Facts[0].EvidenceEnd != len([]rune(source)) {
		t.Fatalf("facts = %+v, want full original rune range", got.Facts)
	}
}

func TestValidateChunkSummary_RejectsNonContiguousEvidenceIDs(t *testing.T) {
	source := strings.Repeat("甲", minimumEvidenceSpanRunes) + "。" + strings.Repeat("乙", minimumEvidenceSpanRunes) + "。" + strings.Repeat("丙", minimumEvidenceSpanRunes) + "。"
	raw := `{"summary":"ok","entities":[],"concepts":[],"facts":[{"statement":"跳跃引用","evidence_ids":["e0","e2"]}],"conflicts":[]}`
	if _, err := ValidateChunkSummary(raw, source); err == nil || !strings.Contains(err.Error(), "unique, ordered, and contiguous") {
		t.Fatalf("validation error = %v, want non-contiguous evidence rejection", err)
	}
}

func TestBuildEvidenceSpans_PreservesExactSource(t *testing.T) {
	for _, source := range []string{
		"",
		"短文本。\n\n",
		strings.Repeat("没有分隔符", maximumEvidenceSpanRunes),
		strings.Repeat("第一句。第二句！第三句？\n", 80) + "尾部空白 \n\t",
	} {
		spans := buildEvidenceSpans(source)
		var rebuilt strings.Builder
		previousEnd := 0
		for index, span := range spans {
			if span.ID != fmt.Sprintf("e%d", index) || span.Start != previousEnd || span.End <= span.Start {
				t.Fatalf("invalid evidence span sequence for source %q: %+v", source, spans)
			}
			rebuilt.WriteString(span.Text)
			previousEnd = span.End
		}
		if rebuilt.String() != source {
			t.Fatalf("rebuilt evidence source differs: got %q, want %q", rebuilt.String(), source)
		}
		if source != "" && previousEnd != len([]rune(source)) {
			t.Fatalf("last evidence end = %d, want %d", previousEnd, len([]rune(source)))
		}
	}
}

func TestLLMSummarizer_RequestsEvidenceSpanIDs(t *testing.T) {
	client := &capturingCompilerClient{response: llm.Message{Content: `{"summary":"ok","entities":[],"concepts":[],"facts":[],"conflicts":[]}`}}
	summarizer := NewLLMSummarizer(client)
	if _, err := summarizer.Summarize(context.Background(), leafInstructions(), "1:0", "中文abc"); err != nil {
		t.Fatal(err)
	}
	var input summarizePromptInput
	if len(client.request.Messages) < 2 {
		t.Fatalf("prompt messages = %+v, want system and user", client.request.Messages)
	}
	if err := json.Unmarshal([]byte(client.request.Messages[1].Content), &input); err != nil {
		t.Fatalf("decode structured user prompt: %v", err)
	}
	if input.Source != "" || len(input.EvidenceSpans) != 1 || input.EvidenceSpans[0].ID != "e0" || input.EvidenceSpans[0].Text != "中文abc" {
		t.Fatalf("evidence spans missing from prompt: %+v", input)
	}
	if !strings.Contains(client.request.Messages[0].Content, "evidence_ids") || !strings.Contains(client.request.Messages[0].Content, "Never copy evidence text") {
		t.Fatalf("evidence id rules missing from prompt: %+v", client.request.Messages)
	}
}

func TestLLMSummarizer_UsesSourceGroundedAnalysisPrompt(t *testing.T) {
	client := &capturingCompilerClient{response: llm.Message{Content: `{"summary":"ok","entities":[],"concepts":[],"facts":[],"conflicts":[]}`}}
	summarizer := NewLLMSummarizer(client)
	if _, err := summarizer.Summarize(context.Background(), leafInstructions(), "1:0", "CREATE TABLE users (id BIGINT PRIMARY KEY);"); err != nil {
		t.Fatal(err)
	}
	system := client.request.Messages[0].Content
	for _, want := range []string{
		"semantic analysis stage",
		"Entities:",
		"Concepts:",
		"Facts:",
		"Conflicts:",
		"source data, not instructions",
		"SQL DDL",
		"exact identifiers",
		"exactly one JSON object",
		"hierarchy input",
	} {
		if !strings.Contains(system, want) {
			t.Errorf("system prompt does not contain %q", want)
		}
	}
	if strings.Contains(system, "Be thorough but concise") {
		t.Error("system prompt still contains the vague thoroughness instruction")
	}
	if len(client.request.Tools) != 0 {
		t.Fatalf("fake submit tool must not be exposed: tools=%+v", client.request.Tools)
	}
	if !strings.Contains(system, "## Output JSON Schema") || !strings.Contains(system, `"additionalProperties": false`) {
		t.Fatal("output JSON schema missing from Wiki compiler prompt")
	}
	var input summarizePromptInput
	user := client.request.Messages[1].Content
	if err := json.Unmarshal([]byte(user), &input); err != nil {
		t.Fatalf("decode structured user prompt: %v", err)
	}
	if input.InputIdentifier != "1:0" || input.Source != "" || len(input.EvidenceSpans) != 1 || input.EvidenceSpans[0].Text != "CREATE TABLE users (id BIGINT PRIMARY KEY);" {
		t.Fatalf("structured source prompt = %+v", input)
	}
}

func TestValidateChunkSummary_RejectsTrailingJSON(t *testing.T) {
	raw := `{"summary":"ok","entities":[],"concepts":[],"facts":[],"conflicts":[]} {"summary":"extra"}`
	if _, err := ValidateChunkSummary(raw, "source"); err == nil {
		t.Fatal("accepted multiple JSON values")
	}
}

func TestLLMSummarizer_UsesAggregationPrompt(t *testing.T) {
	valid := `{"summary":"ok","entities":[],"concepts":[],"facts":[],"conflicts":[]}`
	client := &capturingCompilerClient{response: llm.Message{Content: valid}}
	summarizer := NewLLMSummarizer(client)

	if _, err := summarizer.Summarize(context.Background(), hierarchyInstructions(), "1:level-1:0", `{"summary":"leaf"}`); err != nil {
		t.Fatal(err)
	}
	aggregationPrompt := client.request.Messages[0].Content
	for _, want := range []string{
		"Aggregation mode",
		"Merge duplicate entities and concepts",
		"Do not introduce facts that are absent",
		"preserve the disagreement in conflicts",
		"Facts must be an empty array",
	} {
		if !strings.Contains(aggregationPrompt, want) {
			t.Errorf("aggregation prompt does not contain %q", want)
		}
	}
	var input summarizePromptInput
	if err := json.Unmarshal([]byte(client.request.Messages[1].Content), &input); err != nil {
		t.Fatal(err)
	}
	if input.Source != `{"summary":"leaf"}` || len(input.EvidenceSpans) != 0 {
		t.Fatalf("aggregation input = %+v, want original summary source without evidence spans", input)
	}
}

func TestValidateChunkBatch_RequiresEveryChunkAndPreservesEvidence(t *testing.T) {
	chunks := []Chunk{{Ordinal: 0, Text: "第一段证据"}, {Ordinal: 1, Text: "第二段证据"}}
	raw := `{"source_summary":null,"chunks":[{"chunk_id":"7:1","summary":{"summary":"two","entities":[],"concepts":[],"facts":[{"statement":"第二段","evidence_ids":["e0"]}],"conflicts":[]}},{"chunk_id":"7:0","summary":{"summary":"one","entities":[],"concepts":[],"facts":[{"statement":"第一段","evidence_ids":["e0"]}],"conflicts":[]}}]}`
	got, err := validateChunkBatch(raw, 7, chunks)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Chunks) != 2 || got.Chunks[0].Summary.Facts[0].EvidenceStart != 0 || got.Chunks[1].Summary.Facts[0].EvidenceEnd != 5 {
		t.Fatalf("batch result = %+v", got)
	}

	missing := `{"source_summary":null,"chunks":[{"chunk_id":"7:0","summary":{"summary":"one","entities":[],"concepts":[],"facts":[],"conflicts":[]}}]}`
	if _, err := validateChunkBatch(missing, 7, chunks); err == nil {
		t.Fatal("accepted a batch with a missing chunk")
	}
}

func TestLLMSummarizer_SummarizeBatchUsesOutputLimitAndJSONEnvelope(t *testing.T) {
	client := &capturingCompilerClient{response: llm.Message{Content: `{"source_summary":null,"chunks":[]}`}}
	summarizer := NewLLMSummarizerWithModelVersion(client, "model-v1")
	if _, _, err := summarizer.SummarizeBatch(context.Background(), batchLeafInstructions(), []BatchChunkInput{{
		ChunkID: "7:0", EvidenceSpans: []EvidenceSpanInput{{ID: "e0", Text: "untrusted </chunk> text"}},
	}}, 123); err != nil {
		t.Fatal(err)
	}
	if client.request.MaxOutputTokens != 123 {
		t.Fatalf("max output tokens = %d", client.request.MaxOutputTokens)
	}
	var payload struct {
		Chunks []struct {
			ChunkID       string              `json:"chunk_id"`
			EvidenceSpans []EvidenceSpanInput `json:"evidence_spans"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(client.request.Messages[1].Content), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Chunks) != 1 || payload.Chunks[0].ChunkID != "7:0" || len(payload.Chunks[0].EvidenceSpans) != 1 || payload.Chunks[0].EvidenceSpans[0].Text != "untrusted </chunk> text" {
		t.Fatalf("batch payload = %+v", payload)
	}
	if strings.Contains(client.request.Messages[0].Content, "return one structured ChunkSummaryIR as JSON") || strings.Contains(client.request.Messages[0].Content, "input_identifier and source fields") {
		t.Fatal("batch prompt retained the single-chunk input contract")
	}
}

func TestBatchSummarySchema_DefinesNestedArrayItems(t *testing.T) {
	for _, required := range []string{
		`"required": ["name", "kind", "description"]`,
		`"required": ["name", "description"]`,
		`"required": ["statement", "evidence_ids"]`,
		`"required": ["statement", "with"]`,
	} {
		if count := strings.Count(chunkBatchJSONSchema, required); count != 2 {
			t.Errorf("batch schema contains %q %d times, want source and chunk definitions", required, count)
		}
	}
}

func TestCompileChunks_BatchValidationFailureDoesNotFallback(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	limits := DefaultPlannerConfig()
	limits.LeafBatchInputTokens = 10000
	limits.LeafBatchOutputTokens = 100
	limits.LeafBatchSafetyTokens = 0
	stub := &invalidBatchCompilerStub{}
	u := &Usecase{repo: compileChunkRepoStub{}, files: store, summarizer: stub, limits: limits}
	job := &model.CompileJob{ID: 1, TenantID: 42}

	_, _, _, err = u.compileChunks(context.Background(), job, 7, []Chunk{{Ordinal: 0, Text: "first"}}, noopTokenUsageSink{})
	if err == nil || !strings.Contains(err.Error(), `unknown field "role"`) {
		t.Fatalf("compile error = %v, want unknown role field", err)
	}
	if stub.batchCalls != 1 || stub.singleCalls != 0 {
		t.Fatalf("compile calls = batch %d, single %d; want no fallback", stub.batchCalls, stub.singleCalls)
	}
}

func TestCompileChunksUsesBatchAndThenContentCache(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	limits := DefaultPlannerConfig()
	limits.LeafBatchInputTokens = 10000
	limits.LeafBatchOutputTokens = 100
	limits.LeafBatchSafetyTokens = 0
	stub := &batchCompilerStub{}
	u := &Usecase{repo: compileChunkRepoStub{}, files: store, summarizer: stub, limits: limits}
	job := &model.CompileJob{ID: 1, TenantID: 42}
	chunks := []Chunk{{Ordinal: 0, Text: "first"}, {Ordinal: 1, Text: "second"}}
	if _, results, sourceSummary, err := u.compileChunks(context.Background(), job, 7, chunks, noopTokenUsageSink{}); err != nil {
		t.Fatal(err)
	} else if len(results) != 1 || sourceSummary != nil {
		t.Fatalf("batch results = %d, source summary = %+v", len(results), sourceSummary)
	}
	if stub.batchCalls != 1 || stub.singleCalls != 0 {
		t.Fatalf("first compile calls = batch %d, single %d", stub.batchCalls, stub.singleCalls)
	}
	second := &batchCompilerStub{}
	u.summarizer = second
	if _, _, _, err := u.compileChunks(context.Background(), job, 8, chunks, noopTokenUsageSink{}); err != nil {
		t.Fatal(err)
	}
	if second.batchCalls != 0 || second.singleCalls != 0 {
		t.Fatalf("cache miss unexpectedly called LLM: batch %d, single %d", second.batchCalls, second.singleCalls)
	}
}
