package llm_wiki

import (
	"fmt"
	"strings"
)

// PackChunksByToken keeps source order. Packing is intentionally greedy: the
// compiler needs stable evidence ordering, and the small amount of unused
// space is preferable to reordering chunks across requests.
func PackChunksByToken(chunks []Chunk, maxTokens int) []ChunkBatch {
	if len(chunks) == 0 {
		return nil
	}
	if maxTokens <= 0 {
		maxTokens = 1
	}

	batches := make([]ChunkBatch, 0, len(chunks))
	current := ChunkBatch{}
	for _, chunk := range chunks {
		tokens := chunkBatchTokenCost(chunk)
		if len(current.Chunks) > 0 && current.EstimatedTokens+tokens > maxTokens {
			batches = append(batches, current)
			current = ChunkBatch{}
		}
		current.Chunks = append(current.Chunks, chunk)
		current.EstimatedTokens += tokens
	}
	if len(current.Chunks) > 0 {
		batches = append(batches, current)
	}
	return batches
}

func chunkBatchTokenCost(chunk Chunk) int {
	// Include the generated identifier and evidence-span JSON envelope. The
	// system prompt/schema are reserved by leafBatchInputBudget before packing.
	spans := buildEvidenceSpans(chunk.Text)
	return estimateTokens(fmt.Sprintf(`{"chunk_id":"%d","evidence_spans":%q}`, chunk.Ordinal, chunk.Text)) + len(spans)*12 + 8
}

func leafBatchInputBudget(config PlannerConfig) int {
	overhead := estimateTokens(buildBatchSummarizeSystemPrompt(batchLeafInstructions()))
	budget := config.LeafBatchInputTokens - overhead - config.LeafBatchSafetyTokens
	if budget < 1 {
		return 1
	}
	return budget
}

func chunkContentHash(text string) string {
	return contentSHA256([]byte(text))
}

func (u *Usecase) leafModelVersion(config PlannerConfig) string {
	if value := strings.TrimSpace(config.LeafModelVersion); value != "" && value != "unknown" {
		return value
	}
	return u.summarizerModelVersion()
}

func (u *Usecase) summarizerModelVersion() string {
	if versioned, ok := u.summarizer.(VersionedSummarizer); ok {
		if value := strings.TrimSpace(versioned.ModelVersion()); value != "" {
			return value
		}
	}
	return fmt.Sprintf("%T", u.summarizer)
}
