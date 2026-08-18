package eval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	evidenceGroundingStrong        = "strong"
	evidenceGroundingAbsence       = "absence"
	evidenceGroundingContradiction = "contradiction"
	evidenceGroundingNotApplicable = "not_applicable"
)

var absenceAssertionCuePattern = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(?:not|no|never|without|absent|absence|lack|lacks|free[[:space:]]+of|exclude|excluding|doesn.t|do[[:space:]]+not|cannot|can.t)(?:$|[^\p{L}\p{N}])`)
var artifactFileHeaderPattern = regexp.MustCompile(`^--- file: (.+) \([0-9]+ bytes\) ---$`)
var absenceClauseBoundaryPattern = regexp.MustCompile(`(?i)\s+(?:and|but|while)\s+|[.!?;]+`)

type evidenceGrounding struct {
	Kind        string
	Score       float64
	Span        string
	Observation string
}

func groundAgentEvidence(result AssertionResult, artifactRoot string) (evidenceGrounding, error) {
	if strings.TrimSpace(artifactRoot) == "" {
		return evidenceGrounding{}, fmt.Errorf("%w: agent judge artifact directory is missing", errInvalidGrading)
	}
	if len(result.EvidenceReferences) == 0 {
		return evidenceGrounding{}, fmt.Errorf("%w: passing assertion %q requires at least one positional evidence reference (quote-not-found: cite the exact artifact lines)", errInvalidGrading, result.Text)
	}
	var expected strings.Builder
	locations := make([]string, 0, len(result.EvidenceReferences))
	for index, reference := range result.EvidenceReferences {
		span, err := readAgentEvidenceSpan(artifactRoot, reference)
		if err != nil {
			return evidenceGrounding{}, fmt.Errorf("%w: assertion %q evidence reference %q:%d-%d: %v", errInvalidGrading, result.Text, reference.Path, reference.StartLine, reference.EndLine, err)
		}
		if index > 0 {
			expected.WriteByte('\n')
		}
		expected.WriteString(span)
		locations = append(locations, fmt.Sprintf("%s:%d-%d", filepath.ToSlash(reference.Path), reference.StartLine, reference.EndLine))
	}
	if result.Evidence != expected.String() {
		return evidenceGrounding{}, fmt.Errorf("%w: passing assertion %q evidence does not equal the cited artifact span (quote-not-found: copy the cited lines exactly)", errInvalidGrading, result.Text)
	}
	return evidenceGrounding{Kind: evidenceGroundingStrong, Score: 1, Span: strings.Join(locations, ", "), Observation: result.Evidence}, nil
}

func readAgentEvidenceSpan(root string, reference EvidenceReference) (string, error) {
	if reference.Path == "" || filepath.IsAbs(reference.Path) {
		return "", errors.New("evidence path must be relative")
	}
	clean := filepath.Clean(reference.Path)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.ToSlash(clean) != filepath.ToSlash(reference.Path) {
		return "", errors.New("evidence path escapes or aliases the artifact directory")
	}
	if reference.StartLine < 1 || reference.EndLine < reference.StartLine {
		return "", errors.New("evidence line span is invalid")
	}
	rootAbsolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve artifact directory: %w", err)
	}
	path := filepath.Join(rootAbsolute, clean)
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("read evidence file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("evidence path is not a regular file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read evidence file: %w", err)
	}
	lines := strings.Split(string(contents), "\n")
	if len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if reference.EndLine > len(lines) {
		return "", fmt.Errorf("line %d is outside the file's %d lines", reference.EndLine, len(lines))
	}
	return strings.Join(lines[reference.StartLine-1:reference.EndLine], "\n"), nil
}

func groundAbsence(claim *AbsenceClaim, artifact string) evidenceGrounding {
	if claim == nil {
		return evidenceGrounding{Kind: evidenceGroundingNotApplicable}
	}
	query := normalizeEvidenceText(claim.Query)
	if query == "" {
		return evidenceGrounding{Kind: evidenceGroundingNotApplicable}
	}
	if match, ok := findArtifactQuery(query, artifact); ok {
		return evidenceGrounding{
			Kind:        evidenceGroundingContradiction,
			Score:       1,
			Span:        match.Span,
			Observation: match.Observation,
		}
	}
	return evidenceGrounding{Kind: evidenceGroundingAbsence, Score: 1, Observation: query}
}

func groundDeclaredAbsence(patterns []string, artifact string) evidenceGrounding {
	if len(patterns) == 0 {
		return evidenceGrounding{Kind: evidenceGroundingNotApplicable}
	}
	for _, pattern := range patterns {
		if match, ok := findArtifactQuery(pattern, artifact); ok {
			return evidenceGrounding{
				Kind:        evidenceGroundingContradiction,
				Score:       1,
				Span:        match.Span,
				Observation: match.Observation,
			}
		}
	}
	return evidenceGrounding{Kind: evidenceGroundingAbsence, Score: 1, Observation: strings.Join(patterns, "\n")}
}

type artifactQueryMatch struct {
	Span        string
	Observation string
}

func findArtifactQuery(query, artifact string) (artifactQueryMatch, bool) {
	normalizedQuery := strings.ToLower(normalizeEvidenceText(query))
	if normalizedQuery == "" {
		return artifactQueryMatch{}, false
	}

	currentFile := ""
	sourceLine := 0
	for artifactLine, rawLine := range strings.Split(artifact, "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if header := artifactFileHeaderPattern.FindStringSubmatch(line); header != nil {
			currentFile = header[1]
			sourceLine = 0
		} else if currentFile != "" {
			sourceLine++
		}
		if !strings.Contains(strings.ToLower(normalizeEvidenceText(line)), normalizedQuery) {
			continue
		}

		location := fmt.Sprintf("artifact:%d", artifactLine+1)
		if currentFile != "" && !artifactFileHeaderPattern.MatchString(line) {
			location = fmt.Sprintf("%s:%d", currentFile, sourceLine)
		}
		return artifactQueryMatch{Span: location, Observation: strings.TrimSpace(line)}, true
	}
	return artifactQueryMatch{}, false
}

func normalizeEvidenceText(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\\\n", " ")
	value = strings.ReplaceAll(value, "`", "")
	return strings.Join(strings.Fields(value), " ")
}
