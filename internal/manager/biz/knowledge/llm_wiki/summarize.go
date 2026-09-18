package llm_wiki

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/ongridio/ongrid/internal/pkg/llm"
)

// summarizeTokenBudget is the corpus size (in estimated tokens) above which the
// Map phase summarizes every chunk instead of passing the text through.
const summarizeTokenBudget = 32_000

// digestItem is the plain-text digest of one chunk plus the sources it came
// from. The planner consumes the reduced digest, the evidence resolver maps the
// referenced indices back to source documents.
type digestItem struct {
	ChunkIndex int
	Text       string
	SourceIDs  []uint64
}

// summarizer performs the Map phase of Wiki compilation: it turns every corpus
// chunk into one digest item, summarizing chunks only when the corpus is large
// enough to need it.
type summarizer struct {
	llm CompilerLLM
	log *slog.Logger
}

const summarizerSystemPrompt = `You are a technical documentation summarizer. Output plain text only. Be concise but preserve important technical details.
` + sourceLanguageRule

// newSummarizer creates a corpus summarizer.
func newSummarizer(llm CompilerLLM, log *slog.Logger) *summarizer {
	return &summarizer{llm: llm, log: log}
}

// Summarize turns the corpus into digest items. Short corpora are returned
// verbatim so small installs need no extra LLM round trips.
func (s *summarizer) Summarize(ctx context.Context, corpus *corpus) ([]digestItem, error) {
	// Estimate total tokens
	totalTokens := 0
	for _, chunk := range corpus.Chunks {
		totalTokens += estimateTokens(chunk.Text)
	}

	// Short corpus: skip summarization, use chunks directly
	if totalTokens < summarizeTokenBudget {
		s.log.InfoContext(ctx, "summarizer: short corpus, skipping summarization",
			slog.Int("chunks", len(corpus.Chunks)),
			slog.Int("estimated_tokens", totalTokens))

		items := make([]digestItem, len(corpus.Chunks))
		for i, chunk := range corpus.Chunks {
			items[i] = digestItem{
				ChunkIndex: i,
				Text:       chunk.Text,
				SourceIDs:  []uint64{corpus.Documents[chunk.DocumentIndex].SourceID},
			}
		}
		return items, nil
	}

	// Long corpus: perform Map summarization
	s.log.InfoContext(ctx, "summarizer: performing map summarization",
		slog.Int("chunks", len(corpus.Chunks)),
		slog.Int("estimated_tokens", totalTokens))

	items := make([]digestItem, len(corpus.Chunks))
	for i, chunk := range corpus.Chunks {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		summary, err := s.summarizeChunk(ctx, chunk.Text, i)
		if err != nil {
			return nil, fmt.Errorf("summarize chunk %d: %w", i, err)
		}

		items[i] = digestItem{
			ChunkIndex: i,
			Text:       summary,
			SourceIDs:  []uint64{corpus.Documents[chunk.DocumentIndex].SourceID},
		}
	}

	return items, nil
}

// summarizeChunk generates a plain-text summary of a chunk.
func (s *summarizer) summarizeChunk(ctx context.Context, text string, index int) (string, error) {
	prompt := summarizeChunkPrompt(text)

	resp, err := s.llm.Complete(ctx, llm.ChatReq{
		Messages: []llm.Message{
			{Role: "system", Content: summarizerSystemPrompt},
			{Role: "user", Content: prompt},
		},
	})
	if err != nil {
		return "", fmt.Errorf("llm summarize: %w", err)
	}

	summary := strings.TrimSpace(resp.Assistant.Content)
	if summary == "" {
		return "", fmt.Errorf("empty summary returned for chunk %d", index)
	}

	return summary, nil
}

// summarizeChunkPrompt renders the summary request for one source chunk.
func summarizeChunkPrompt(text string) string {
	return fmt.Sprintf(`Summarize the following technical documentation in plain language.

Requirements:
- Focus on key facts, concepts, procedures, and configurations
- Preserve important details like function names, parameters, and values
- Keep the summary concise but complete
- Output plain text only, no JSON or markdown formatting
- %s

Text to summarize:
%s`, sourceLanguageRule, text)
}

// buildDigest joins digest items into the single digest string the planner reads.
func buildDigest(items []digestItem) string {
	var digest strings.Builder
	for i, item := range items {
		// Only expose the stable, zero-based digest index. Exposing database
		// source IDs here made the planner confuse those IDs with item indices.
		digest.WriteString(fmt.Sprintf("--- Digest source_id: %d ---\n", i))
		digest.WriteString(item.Text)
		digest.WriteString("\n\n")
	}
	return digest.String()
}

// estimateTokens provides a rough token estimate (charsPerToken characters per token).
func estimateTokens(text string) int {
	return len(text) / charsPerToken
}
