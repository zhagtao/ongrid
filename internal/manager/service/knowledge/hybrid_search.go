// Package knowledge contains application services that compose the
// operator knowledge base with the compiled LLM Wiki.
package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strings"

	knowledgebiz "github.com/ongridio/ongrid/internal/manager/biz/knowledge"
	llmwikibiz "github.com/ongridio/ongrid/internal/manager/biz/knowledge/llm_wiki"
	knowledgemodel "github.com/ongridio/ongrid/internal/manager/model/knowledge"
)

// RawSearcher is the part of the operator knowledge base needed by the
// hybrid search service.
type RawSearcher interface {
	Search(ctx context.Context, query string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error)
}

// HybridSearcher merges compiled Wiki hits with the existing Raw knowledge
// base. It is shared by the Agent tool and the Knowledge HTTP API.
type HybridSearcher struct {
	raw  RawSearcher
	wiki *llmwikibiz.Usecase
}

// NewHybridSearcher creates a searcher. wiki may be nil when the LLM Wiki
// feature is disabled; Raw search remains available in that case.
func NewHybridSearcher(raw RawSearcher, wiki *llmwikibiz.Usecase) *HybridSearcher {
	return &HybridSearcher{raw: raw, wiki: wiki}
}

// Search implements the application-level hybrid retrieval policy.
func (s *HybridSearcher) Search(ctx context.Context, query string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error) {
	if opts.Limit <= 0 {
		opts.Limit = 10
	}
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = "hybrid"
	}
	if mode == "rag" || s.wiki == nil {
		opts.Mode = ""
		return s.raw.Search(ctx, query, opts)
	}

	wikiHits, wikiErr := s.wiki.Search(ctx, llmwikibiz.DefaultTenantID, query, opts.Limit)
	converted := make([]knowledgebiz.SearchHit, 0, len(wikiHits))
	for _, hit := range wikiHits {
		digest := sha256.Sum256([]byte(hit.PageID))
		converted = append(converted, knowledgebiz.SearchHit{
			Doc: &knowledgemodel.Doc{
				ID:         binary.BigEndian.Uint64(digest[:8]),
				SourceType: "wiki",
				Title:      hit.Title,
				Content:    hit.Preview,
			},
			Score:           hit.Score,
			Layer:           hit.Layer,
			PageType:        hit.PageType,
			PageID:          hit.PageID,
			SourceVersionID: hit.SourceVersionID,
			MatchedNode:     hit.MatchedNode,
		})
	}
	if mode == "wiki" {
		return converted, wikiErr
	}

	opts.Mode = ""
	rawHits, rawErr := s.raw.Search(ctx, query, opts)
	if wikiErr != nil && rawErr != nil {
		return nil, errors.Join(wikiErr, rawErr)
	}
	if wikiErr != nil {
		return rawHits, nil
	}
	if rawErr != nil {
		return converted, nil
	}
	for rank := range converted {
		converted[rank].Score = llmwikibiz.DefaultWikiWeight / float64(60+rank+1)
	}
	for rank := range rawHits {
		rawHits[rank].Score = llmwikibiz.DefaultRawWeight / float64(60+rank+1)
		if rawHits[rank].Layer == "" {
			rawHits[rank].Layer = "raw"
		}
	}
	merged := append(converted, rawHits...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > opts.Limit {
		merged = merged[:opts.Limit]
	}
	return merged, nil
}
