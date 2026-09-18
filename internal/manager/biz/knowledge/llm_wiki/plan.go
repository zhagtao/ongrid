package llm_wiki

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/ongridio/ongrid/internal/pkg/llm"
)

// plan is the page structure the LLM proposes for one build.
type plan struct {
	Pages []planPage `json:"pages"`
}

// planPage is one page the planner wants written.
type planPage struct {
	PageID   string        `json:"page_id"`
	Title    string        `json:"title"`
	Sections []planSection `json:"sections"`
}

// planSection is one section of a planned page. SourceIDs point at digest items
// so the evidence resolver can trace the section back to raw documents.
type planSection struct {
	Heading   string `json:"heading"`
	Content   string `json:"content"`
	SourceIDs []int  `json:"source_ids"`
}

// planResponseSchema is the strict JSON Schema the planner asks providers for.
// Providers that cannot honor it fall back to json_object mode.
const planResponseSchema = `{
	"type": "object",
	"properties": {
		"pages": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"page_id": {
						"type": "string",
						"description": "Unique identifier for the page, using kebab-case"
					},
					"title": {
						"type": "string",
						"description": "Human-readable page title"
					},
					"sections": {
						"type": "array",
						"items": {
							"type": "object",
							"properties": {
								"heading": {
									"type": "string",
									"description": "Section heading"
								},
								"content": {
									"type": "string",
									"description": "Section content in Markdown format"
								},
								"source_ids": {
									"type": "array",
									"items": {"type": "integer", "minimum": 0},
									"description": "Zero-based Digest source_id indices that provide source material; these are not database IDs"
								}
							},
							"required": ["heading", "content", "source_ids"]
						}
					}
				},
				"required": ["page_id", "title", "sections"]
			}
		}
	},
	"required": ["pages"]
}`

// plannerSystemPrompt is the system prompt for the planning stage.
const plannerSystemPrompt = `You are a Wiki structure planner for technical documentation.

Your task is to analyze a digest of documentation and create a well-organized Wiki structure.

Guidelines:
1. Each page should cover a coherent, self-contained topic
2. Pages should be logically ordered and cross-referenced where appropriate
3. Sections within a page should follow a logical flow
4. Preserve important technical details, code examples, and configurations
5. Use clear, descriptive page IDs in kebab-case format
6. source_ids MUST contain only the zero-based "Digest source_id" values printed in the digest; never invent or use database IDs

` + sourceLanguageRule + `

Output valid JSON only. Do not include any explanations or markdown.`

// planner turns the reduced corpus digest into the page structure of a build.
type planner struct {
	llm CompilerLLM
	log *slog.Logger
}

// newPlanner creates a Wiki structure planner.
func newPlanner(llm CompilerLLM, log *slog.Logger) *planner {
	return &planner{llm: llm, log: log}
}

// Plan asks the LLM for a page structure and validates it locally. It asks for a
// strict JSON Schema first and falls back to json_object mode when the provider
// refuses the schema.
func (p *planner) Plan(ctx context.Context, digest string) (*plan, error) {
	p.log.InfoContext(ctx, "planner: planning Wiki structure", slog.Int("digest_length", len(digest)))

	created, err := p.planWithJSONSchema(ctx, digest)
	if err != nil {
		p.log.WarnContext(ctx, "planner: JSON schema planning failed, trying json_object",
			slog.String("error", err.Error()))
		created, err = p.planWithJSONObject(ctx, digest)
		if err != nil {
			return nil, fmt.Errorf("planner failed: %w", err)
		}
	}

	if err := validatePlan(created); err != nil {
		return nil, fmt.Errorf("invalid plan: %w", err)
	}

	p.log.InfoContext(ctx, "planner: plan created", slog.Int("pages", len(created.Pages)))
	return created, nil
}

// planWithJSONSchema asks for the plan with a JSON Schema response format.
func (p *planner) planWithJSONSchema(ctx context.Context, digest string) (*plan, error) {
	return p.requestPlan(ctx, digest, &llm.ResponseFormat{
		Type:   llm.ResponseFormatJSONSchema,
		Name:   "wiki_plan",
		Schema: json.RawMessage(planResponseSchema),
	})
}

// planWithJSONObject asks for the plan in plain json_object mode.
func (p *planner) planWithJSONObject(ctx context.Context, digest string) (*plan, error) {
	return p.requestPlan(ctx, digest, &llm.ResponseFormat{Type: llm.ResponseFormatJSONObject})
}

// requestPlan performs one planning request and decodes the response. Both
// response-format attempts share it so they stay identical apart from the format.
func (p *planner) requestPlan(ctx context.Context, digest string, format *llm.ResponseFormat) (*plan, error) {
	resp, err := p.llm.Complete(ctx, llm.ChatReq{
		Messages: []llm.Message{
			{Role: "system", Content: plannerSystemPrompt},
			{Role: "user", Content: plannerPrompt(digest)},
		},
		ResponseFormat: format,
	})
	if err != nil {
		return nil, err
	}

	var created plan
	if err := json.Unmarshal([]byte(resp.Assistant.Content), &created); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	return &created, nil
}

// validatePlan rejects structurally unusable plans before any page is written.
func validatePlan(created *plan) error {
	if created == nil {
		return fmt.Errorf("nil plan")
	}
	if len(created.Pages) == 0 {
		return fmt.Errorf("plan has no pages")
	}

	seenPageIDs := make(map[string]bool)
	for i, page := range created.Pages {
		if page.PageID == "" {
			return fmt.Errorf("page %d has empty page_id", i)
		}
		if page.Title == "" {
			return fmt.Errorf("page %d has empty title", i)
		}
		if seenPageIDs[page.PageID] {
			return fmt.Errorf("duplicate page_id: %s", page.PageID)
		}
		seenPageIDs[page.PageID] = true

		if len(page.Sections) == 0 {
			return fmt.Errorf("page %s has no sections", page.PageID)
		}

		for j, section := range page.Sections {
			if section.Heading == "" {
				return fmt.Errorf("page %s section %d has empty heading", page.PageID, j)
			}
			if section.Content == "" {
				return fmt.Errorf("page %s section %d has empty content", page.PageID, j)
			}
		}
	}

	return nil
}

// plannerPrompt renders the user prompt for planning.
func plannerPrompt(digest string) string {
	return fmt.Sprintf(`Based on the following digest of technical documentation, plan a Wiki structure.

Requirements:
- Create pages that cover distinct topics
- Each page should have well-organized sections
- Include source_ids using only the zero-based "Digest source_id" values shown below
- Pages should be self-contained but may reference other pages
- %s

Digest of documentation:
%s

Output a JSON object with a "pages" array. Each page has:
- "page_id": unique identifier in kebab-case
- "title": human-readable title
- "sections": array of sections, each with "heading", "content" (Markdown), and "source_ids" (array of zero-based Digest source_id integers)`, sourceLanguageRule, digest)
}
