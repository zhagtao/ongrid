package llm_wiki

import "strings"

type PageQuality struct {
	ContentBytes   int
	SectionCount   int
	EvidenceTokens int
	FactCount      int
	ChunkCount     int
	SourceCount    int
}

func evaluatePageQuality(article *WikiArticle, topic TopicPlan) PageQuality {
	quality := PageQuality{
		EvidenceTokens: topic.EvidenceTokens, FactCount: topic.FactCount,
		ChunkCount: topic.ChunkCount, SourceCount: topic.SourceCount,
	}
	if article == nil {
		return quality
	}
	quality.ContentBytes = len(article.Summary)
	for _, section := range article.Sections {
		if strings.TrimSpace(section.Body) == "" {
			continue
		}
		quality.SectionCount++
		quality.ContentBytes += len(section.Title) + len(section.Body)
	}
	for _, limitation := range article.Limitations {
		quality.ContentBytes += len(limitation)
	}
	return quality
}

func acceptTopicPage(quality PageQuality) bool {
	if quality.SectionCount >= 2 {
		return true
	}
	return quality.SourceCount >= 2 && quality.ContentBytes >= 600
}

func shouldPromoteTopic(topic TopicPlan, config PlannerConfig) bool {
	if topic.Importance < config.MinImportance {
		return false
	}
	return topic.SourceCount >= config.MinSourceCount ||
		topic.ChunkCount >= config.MinChunkCount ||
		topic.FactCount >= config.MinFactCount ||
		topic.EvidenceTokens >= config.MinEvidenceTokens
}

func reviewTopicPage(article *WikiArticle, topic TopicPlan) PageReview {
	quality := evaluatePageQuality(article, topic)
	if acceptTopicPage(quality) {
		return PageReview{Decision: PageReviewKeep, Reason: "deterministic quality gate passed"}
	}
	return PageReview{Decision: PageReviewDrop, Reason: "insufficient independent sections or supported content"}
}
