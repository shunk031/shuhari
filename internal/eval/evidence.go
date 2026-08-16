package eval

import (
	"regexp"
	"strings"
)

const (
	evidenceGroundingStrong        = "strong"
	evidenceGroundingParaphrase    = "paraphrase"
	evidenceGroundingHallucination = "hallucination"
	evidenceGroundingNotApplicable = "not_applicable"

	paraphraseGroundingThreshold = 0.75
	minimumParaphraseTokens      = 8
	artifactWindowRatio          = 2
)

var groundingTokenPattern = regexp.MustCompile(`[\p{L}\p{N}]+`)

type evidenceGrounding struct {
	Kind  string
	Score float64
	Span  string
}

type groundingToken struct {
	Value string
	Start int
	End   int
}

func groundEvidence(evidence, artifact string) evidenceGrounding {
	normalizedArtifact := normalizeEvidenceText(artifact)
	observations := quotedObservations(evidence)
	for _, observation := range observations {
		quoted := normalizeEvidenceText(decodeQuotedObservation(observation))
		if quoted != "" && strings.Contains(normalizedArtifact, quoted) {
			return evidenceGrounding{Kind: evidenceGroundingStrong, Score: 1, Span: quoted}
		}
	}

	best := evidenceGrounding{Kind: evidenceGroundingHallucination}
	for _, observation := range observations {
		quoted := normalizeEvidenceText(decodeQuotedObservation(observation))
		candidate := groundParaphrase(quoted, normalizedArtifact)
		if candidate.Score > best.Score {
			best = candidate
		}
	}
	if best.Score >= paraphraseGroundingThreshold {
		best.Kind = evidenceGroundingParaphrase
		return best
	}
	return evidenceGrounding{Kind: evidenceGroundingHallucination, Score: best.Score, Span: best.Span}
}

func groundParaphrase(quote, artifact string) evidenceGrounding {
	quoteTokens := tokenizeGroundingText(quote)
	artifactTokens := tokenizeGroundingText(artifact)
	if len(quoteTokens) < minimumParaphraseTokens || len(artifactTokens) == 0 {
		return evidenceGrounding{Kind: evidenceGroundingHallucination}
	}

	maxWindow := artifactWindowRatio * len(quoteTokens)
	bestMatches := 0
	bestStart, bestEnd := 0, 0
	for start := 0; start < len(artifactTokens); start++ {
		end := min(start+maxWindow, len(artifactTokens))
		matches := longestCommonTokenSubsequence(quoteTokens, artifactTokens[start:end])
		if len(matches) == 0 {
			continue
		}
		spanStart := start + matches[0]
		spanEnd := start + matches[len(matches)-1]
		spanWidth := artifactTokens[spanEnd].End - artifactTokens[spanStart].Start
		bestWidth := 0
		if bestMatches > 0 {
			bestWidth = artifactTokens[bestEnd].End - artifactTokens[bestStart].Start
		}
		if len(matches) > bestMatches || (len(matches) == bestMatches && (bestWidth == 0 || spanWidth < bestWidth)) {
			bestMatches = len(matches)
			bestStart = spanStart
			bestEnd = spanEnd
		}
	}
	if bestMatches == 0 {
		return evidenceGrounding{Kind: evidenceGroundingHallucination}
	}
	return evidenceGrounding{
		Kind:  evidenceGroundingHallucination,
		Score: float64(bestMatches) / float64(len(quoteTokens)),
		Span:  artifact[artifactTokens[bestStart].Start:artifactTokens[bestEnd].End],
	}
}

func tokenizeGroundingText(value string) []groundingToken {
	indices := groundingTokenPattern.FindAllStringIndex(value, -1)
	tokens := make([]groundingToken, 0, len(indices))
	for _, index := range indices {
		tokens = append(tokens, groundingToken{Value: strings.ToLower(value[index[0]:index[1]]), Start: index[0], End: index[1]})
	}
	return tokens
}

func longestCommonTokenSubsequence(reference, candidate []groundingToken) []int {
	table := make([][]int, len(reference)+1)
	for index := range table {
		table[index] = make([]int, len(candidate)+1)
	}
	for row := 1; row <= len(reference); row++ {
		for column := 1; column <= len(candidate); column++ {
			if reference[row-1].Value == candidate[column-1].Value {
				table[row][column] = table[row-1][column-1] + 1
			} else {
				table[row][column] = max(table[row-1][column], table[row][column-1])
			}
		}
	}

	matches := make([]int, 0, table[len(reference)][len(candidate)])
	for row, column := len(reference), len(candidate); row > 0 && column > 0; {
		if reference[row-1].Value == candidate[column-1].Value {
			matches = append(matches, column-1)
			row--
			column--
		} else if table[row][column-1] > table[row-1][column] {
			column--
		} else {
			row--
		}
	}
	for left, right := 0, len(matches)-1; left < right; left, right = left+1, right-1 {
		matches[left], matches[right] = matches[right], matches[left]
	}
	return matches
}

func decodeQuotedObservation(value string) string {
	var decoded strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' || index+1 == len(value) {
			decoded.WriteByte(value[index])
			continue
		}
		if strings.HasPrefix(value[index:], `\\\n`) {
			decoded.WriteByte('\\')
			decoded.WriteByte('\n')
			index += 3
			continue
		}
		switch value[index+1] {
		case 'n':
			decoded.WriteByte('\n')
		case 't':
			decoded.WriteByte('\t')
		case '"':
			decoded.WriteByte('"')
		default:
			decoded.WriteByte(value[index])
			continue
		}
		index++
	}
	return decoded.String()
}

func normalizeEvidenceText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\\\n", " ")
	value = strings.ReplaceAll(value, "`", "")
	return strings.Join(strings.Fields(value), " ")
}

func quotedObservations(evidence string) []string {
	runes := []rune(evidence)
	observations := make([]string, 0, 1)
	for index := 0; index < len(runes); index++ {
		opening := runes[index]
		closing := rune(0)
		switch opening {
		case '"':
			closing = '"'
		case '“':
			closing = '”'
		default:
			continue
		}
		for end := index + 1; end < len(runes); end++ {
			if runes[end] != closing || (closing == '"' && escapedQuote(runes, end)) {
				continue
			}
			observations = append(observations, string(runes[index+1:end]))
			index = end
			break
		}
	}
	return observations
}

func escapedQuote(value []rune, index int) bool {
	backslashes := 0
	for index--; index >= 0 && value[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}
