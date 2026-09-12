package llm_wiki

import "strings"

const charsPerToken = 4

func SplitMarkdown(text string) []Chunk {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	target, hard := TargetChunkTokens*charsPerToken, HardChunkTokens*charsPerToken
	var out []Chunk
	start := 0
	for start < len(runes) {
		end := start + target
		if end > len(runes) {
			end = len(runes)
		}
		if end < len(runes) {
			end = structuralBoundary(runes, start, end, hard)
		}
		if end <= start {
			end = start + hard
			if end > len(runes) {
				end = len(runes)
			}
		}
		out = append(out, Chunk{Ordinal: len(out), Start: start, End: end, Text: string(runes[start:end])})
		if end == len(runes) {
			break
		}
		overlap := (end - start) / 10
		if overlap > 400*charsPerToken {
			overlap = 400 * charsPerToken
		}
		start = end - overlap
	}
	return out
}

func structuralBoundary(runes []rune, start, target, hard int) int {
	limit := start + hard
	if limit > len(runes) {
		limit = len(runes)
	}
	segment := string(runes[start:limit])
	targetRel := target - start
	for _, marker := range []string{"\n#", "\n\n", "\n- ", "\n", "。", ". "} {
		positions := runePositions(segment, marker)
		best := -1
		for _, p := range positions {
			if p <= targetRel {
				best = p + len([]rune(marker))
			} else if best < 0 {
				best = p + len([]rune(marker))
			}
		}
		if best > 0 {
			return start + best
		}
	}
	return limit
}

func runePositions(s, marker string) []int {
	var out []int
	offset := 0
	for {
		i := strings.Index(s, marker)
		if i < 0 {
			break
		}
		offset += len([]rune(s[:i]))
		out = append(out, offset)
		consumed := i + len(marker)
		offset += len([]rune(marker))
		s = s[consumed:]
	}
	return out
}
