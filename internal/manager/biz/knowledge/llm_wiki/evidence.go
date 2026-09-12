package llm_wiki

import (
	"fmt"
	"unicode"
)

const (
	minimumEvidenceSpanRunes = 48
	maximumEvidenceSpanRunes = 320
)

// evidenceSpan is an exact, compiler-owned slice of a source chunk. Only ID
// and Text are sent to the model; rune offsets remain trusted server state.
type evidenceSpan struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Start int    `json:"-"`
	End   int    `json:"-"`
}

func buildEvidenceSpans(source string) []evidenceSpan {
	runes := []rune(source)
	if len(runes) == 0 {
		return nil
	}

	spans := make([]evidenceSpan, 0, len(runes)/minimumEvidenceSpanRunes+1)
	for start := 0; start < len(runes); {
		end := evidenceSpanEnd(runes, start)
		if end == len(runes) && whitespaceOnly(runes[start:end]) && len(spans) > 0 {
			last := &spans[len(spans)-1]
			last.Text += string(runes[start:end])
			last.End = end
			break
		}
		spans = append(spans, evidenceSpan{
			ID:    fmt.Sprintf("e%d", len(spans)),
			Text:  string(runes[start:end]),
			Start: start,
			End:   end,
		})
		start = end
	}
	return spans
}

func evidenceSpanInputs(source string) []EvidenceSpanInput {
	spans := buildEvidenceSpans(source)
	inputs := make([]EvidenceSpanInput, 0, len(spans))
	for _, span := range spans {
		inputs = append(inputs, EvidenceSpanInput{ID: span.ID, Text: span.Text})
	}
	return inputs
}

func evidenceSpanEnd(runes []rune, start int) int {
	limit := start + maximumEvidenceSpanRunes
	if limit > len(runes) {
		limit = len(runes)
	}
	minimum := start + minimumEvidenceSpanRunes
	for index := start; index < limit; index++ {
		if index+1 >= minimum && isEvidenceBoundary(runes, index) {
			return index + 1
		}
	}
	return limit
}

func isEvidenceBoundary(runes []rune, index int) bool {
	switch runes[index] {
	case '\n', '。', '！', '？', '!', '?':
		return true
	case '.', ';':
		return index+1 == len(runes) || unicode.IsSpace(runes[index+1])
	default:
		return false
	}
}

func whitespaceOnly(runes []rune) bool {
	for _, value := range runes {
		if !unicode.IsSpace(value) {
			return false
		}
	}
	return true
}
