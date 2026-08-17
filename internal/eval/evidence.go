package eval

import (
	"regexp"
	"strings"
)

const (
	evidenceGroundingStrong        = "strong"
	evidenceGroundingParaphrase    = "paraphrase"
	evidenceGroundingAbsence       = "absence"
	evidenceGroundingHallucination = "hallucination"
	evidenceGroundingNotApplicable = "not_applicable"

	paraphraseGroundingThreshold = 0.75
	minimumParaphraseTokens      = 8
	artifactWindowRatio          = 2
)

var groundingTokenPattern = regexp.MustCompile(`[\p{L}\p{N}]+`)
var absenceAssertionCuePattern = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:not|no|never|without|absent|absence|lack|lacks|free[[:space:]]+of|exclude|excluding|doesn.t|do[[:space:]]+not|cannot|can.t)(?:$|[^\p{L}\p{N}])`)

var observationQuotePairs = map[rune]rune{
	'"': '"',
	'“': '”',
	'‘': '’',
}

type evidenceGrounding struct {
	Kind        string
	Score       float64
	Span        string
	Observation string
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
			return evidenceGrounding{Kind: evidenceGroundingStrong, Score: 1, Span: quoted, Observation: quoted}
		}
	}

	best := evidenceGrounding{Kind: evidenceGroundingHallucination}
	for _, observation := range observations {
		quoted := normalizeEvidenceText(decodeQuotedObservation(observation))
		candidate := groundParaphrase(quoted, normalizedArtifact)
		candidate.Observation = quoted
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

func groundAbsence(claim *AbsenceClaim, artifact string) evidenceGrounding {
	if claim == nil {
		return evidenceGrounding{Kind: evidenceGroundingHallucination}
	}
	query := normalizeEvidenceText(claim.Query)
	if query == "" || len(tokenizeGroundingText(query)) == 0 {
		return evidenceGrounding{Kind: evidenceGroundingHallucination}
	}
	if strings.Contains(strings.ToLower(normalizeEvidenceText(artifact)), strings.ToLower(query)) {
		return evidenceGrounding{Kind: evidenceGroundingHallucination, Observation: query}
	}
	return evidenceGrounding{Kind: evidenceGroundingAbsence, Score: 1, Observation: query}
}

func supportsAbsenceClaim(assertion string, query string) bool {
	normalizedAssertion := strings.ToLower(normalizeEvidenceText(assertion))
	normalizedQuery := strings.ToLower(normalizeEvidenceText(query))
	if normalizedQuery == "" || !strings.Contains(normalizedAssertion, normalizedQuery) {
		return false
	}
	return absenceAssertionCuePattern.MatchString(normalizedAssertion)
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
	outer := outerQuotedObservations(evidence)
	if len(outer) == 0 {
		return nil
	}
	observations := make([]string, 0, len(outer)+2)
	seen := map[string]struct{}{}
	appendObservation := func(observation string) {
		if _, exists := seen[observation]; exists {
			return
		}
		seen[observation] = struct{}{}
		observations = append(observations, observation)
	}
	for _, observation := range outer {
		appendObservation(observation)
	}
	for _, scope := range append([]string{evidence}, outer...) {
		for _, observation := range backtickObservations(scope) {
			appendObservation(observation)
		}
		for _, observation := range escapedDoubleQuotedObservations(scope) {
			appendObservation(observation)
		}
		for _, observation := range smartQuotedObservations(scope) {
			appendObservation(observation)
		}
	}
	return observations
}

func outerQuotedObservations(evidence string) []string {
	runes := []rune(evidence)
	observations := make([]string, 0, 1)
	for index := 0; index < len(runes); index++ {
		opening := runes[index]
		closing, ok := observationQuotePairs[opening]
		if !ok {
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

func smartQuotedObservations(value string) []string {
	runes := []rune(value)
	observations := []string{}
	for index := 0; index < len(runes); index++ {
		opening := runes[index]
		if opening == '"' {
			continue
		}
		closing, ok := observationQuotePairs[opening]
		if !ok {
			continue
		}
		for end := index + 1; end < len(runes); end++ {
			if runes[end] != closing {
				continue
			}
			observation := string(runes[index+1 : end])
			observations = append(observations, observation)
			if trimmed := strings.TrimSuffix(observation, ","); trimmed != observation {
				observations = append(observations, trimmed)
			}
			index = end
			break
		}
	}
	return observations
}

func backtickObservations(value string) []string {
	observations := []string{}
	for index := 0; index < len(value); {
		if value[index] != '`' {
			index++
			continue
		}
		delimiterStart := index
		for index < len(value) && value[index] == '`' {
			index++
		}
		delimiter := value[delimiterStart:index]
		closingOffset := strings.Index(value[index:], delimiter)
		if closingOffset < 0 {
			continue
		}
		closingStart := index + closingOffset
		observations = append(observations, value[index:closingStart])
		index = closingStart + len(delimiter)
	}
	return observations
}

func escapedDoubleQuotedObservations(value string) []string {
	runes := []rune(value)
	observations := []string{}
	for index := 0; index < len(runes); index++ {
		if runes[index] != '"' || !escapedQuote(runes, index) {
			continue
		}
		for end := index + 1; end < len(runes); end++ {
			if runes[end] != '"' || !escapedQuote(runes, end) {
				continue
			}
			observations = append(observations, string(runes[index+1:end-1]))
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
