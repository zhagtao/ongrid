package llm_wiki

import (
	"strings"
	"testing"
)

func TestReviewTopicPage_DropsThinSingleSection(t *testing.T) {
	article := &WikiArticle{Summary: "short", Sections: []WikiSection{{Title: "Overview", Body: strings.Repeat("x", 80)}}}
	review := reviewTopicPage(article, TopicPlan{EvidenceTokens: 200, ChunkCount: 1, SourceCount: 1})
	if review.Decision != PageReviewDrop {
		t.Fatalf("review = %+v", review)
	}
}

func TestReviewTopicPage_KeepsStructuredArticle(t *testing.T) {
	article := &WikiArticle{Summary: "grounded", Sections: []WikiSection{
		{Title: "Overview", Body: "definition"},
		{Title: "Implementation", Body: "details"},
		{Title: "Limitations", Body: "constraints"},
	}}
	review := reviewTopicPage(article, TopicPlan{EvidenceTokens: 3000, ChunkCount: 3, SourceCount: 1})
	if review.Decision != PageReviewKeep {
		t.Fatalf("review = %+v", review)
	}
}
