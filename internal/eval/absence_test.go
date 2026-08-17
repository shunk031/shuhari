package eval

import "testing"

func TestBuildGradingAcceptsStructuredAbsenceClaim(t *testing.T) {
	t.Parallel()

	artifact := "updated_code.py contains only the requested code change."
	grading, err := buildGrading(
		[]string{"the response does not introduce a document review step"},
		[]AssertionResult{{
			Text:     "the response does not introduce a document review step",
			Passed:   true,
			Evidence: `No occurrence of "document review step" appears in the artifact.`,
			Absence:  &AbsenceClaim{NegatedClause: "does not introduce a document review step", Query: "document review step", Rationale: "The query names the forbidden review step."},
		}},
		artifact,
	)
	if err != nil {
		t.Fatalf("buildGrading() rejected structured absence claim: %v", err)
	}
	result := grading.AssertionResults[0]
	if result.EvidenceGrounding != evidenceGroundingAbsence || result.EvidenceGroundingScore != 1 {
		t.Fatalf("absence grounding = %q score %v, want absence/1", result.EvidenceGrounding, result.EvidenceGroundingScore)
	}
	if result.EvidenceGroundingObservation != "document review step" {
		t.Fatalf("absence observation = %q", result.EvidenceGroundingObservation)
	}
}

func TestBuildGradingAcceptsAbsenceClaimForMixedAssertion(t *testing.T) {
	t.Parallel()

	assertion := "The response uses an explicit HTTPS push URL and does not approve the pull request as the bot."
	query := "gh pr review --approve --repo creative-graphic-design/design-generators"
	artifact := "git remote set-url --push origin https://github.com/creative-graphic-design/design-generators The bot must not approve its own pull request."
	grading, err := buildGrading(
		[]string{assertion},
		[]AssertionResult{{
			Text:     assertion,
			Passed:   true,
			Evidence: `"git remote set-url --push origin https://github.com/creative-graphic-design/design-generators The bot must not approve its own pull request."`,
			Absence:  &AbsenceClaim{NegatedClause: "does not approve the pull request as the bot", Query: query, Rationale: "The command query checks the approval action forbidden by the clause."},
		}},
		artifact,
	)
	if err != nil {
		t.Fatalf("buildGrading() rejected mixed assertion absence claim: %v", err)
	}
	result := grading.AssertionResults[0]
	if result.EvidenceGrounding != evidenceGroundingAbsence || result.EvidenceGroundingObservation != query {
		t.Fatalf("mixed absence grounding = %q (%q), want absence (%q)", result.EvidenceGrounding, result.EvidenceGroundingObservation, query)
	}
}

func TestBuildGradingResolvesContradictedAbsenceAsFailed(t *testing.T) {
	t.Parallel()

	assertion := "The response does not use gh auth switch as the runtime account-selection mechanism."
	artifact := `
--- file: prepare-cgd-auth.sh (1236 bytes) ---
#!/usr/bin/env bash
set -euo pipefail

expected_account="creative-graphic-design-dev"
github_host="github.com"
cgd_repo="creative-graphic-design/design-generators"

if ! command -v gh >/dev/null 2>&1; then
  printf '%s\n' "GitHub CLI (gh) is required. Install it, then rerun this script." >&2
  exit 1
fi

printf '%s\n' "Opening GitHub's supported browser authentication flow..."
gh auth login \
  --hostname "$github_host" \
  --git-protocol https \
  --web

actual_account="$(gh api user --jq '.login')"
if [[ "$actual_account" != "$expected_account" ]]; then
  if gh auth switch --hostname "$github_host" --user "$expected_account" >/dev/null 2>&1; then
    actual_account="$(gh api user --jq '.login')"
  fi
fi

if [[ "$actual_account" != "$expected_account" ]]; then
  printf '%s\n' "Authenticated as '$actual_account'; expected '$expected_account'." >&2
  printf '%s\n' "Run: gh auth switch --hostname github.com --user $expected_account" >&2
  exit 1
fi

gh auth setup-git --hostname "$github_host"
gh auth status --hostname "$github_host"
gh repo view "$cgd_repo" --json nameWithOwner,viewerPermission,url \
  --jq '{nameWithOwner, viewerPermission, url}'

printf '%s\n' "CGD authentication is ready for HTTPS repository work."
`
	grading, err := buildGrading(
		[]string{assertion},
		[]AssertionResult{{
			Text:     assertion,
			Passed:   true,
			Evidence: "\"Configured `creative-graphic-design-dev` authentication via GitHub CLI with HTTPS Git credentials. It contains no gh auth switch command.\"",
			Absence:  &AbsenceClaim{NegatedClause: "does not use gh auth switch as the runtime account-selection mechanism", Query: "gh auth switch", Rationale: "The query is the command forbidden by the assertion."},
		}},
		artifact,
	)
	if err != nil {
		t.Fatalf("buildGrading() rejected deterministic contradiction: %v", err)
	}
	result := grading.AssertionResults[0]
	if result.Passed {
		t.Fatal("contradicted absence assertion remained passed")
	}
	if result.EvidenceGrounding != evidenceGroundingContradiction {
		t.Fatalf("contradiction grounding = %q, want %q", result.EvidenceGrounding, evidenceGroundingContradiction)
	}
	if result.EvidenceGroundingSpan != "prepare-cgd-auth.sh:21" {
		t.Fatalf("contradiction span = %q, want prepare-cgd-auth.sh:21", result.EvidenceGroundingSpan)
	}
	if result.EvidenceGroundingObservation != "if gh auth switch --hostname \"$github_host\" --user \"$expected_account\" >/dev/null 2>&1; then" {
		t.Fatalf("contradiction observation = %q", result.EvidenceGroundingObservation)
	}
	if grading.Summary.Passed != 0 || grading.Summary.Failed != 1 {
		t.Fatalf("summary = %#v, want one failed assertion", grading.Summary)
	}
}

func TestBuildGradingRejectsNonVerbatimMixedAssertionAbsenceClause(t *testing.T) {
	t.Parallel()

	assertion := "The response uses an explicit HTTPS push URL and does not approve the pull request as the bot."
	_, err := buildGrading(
		[]string{assertion},
		[]AssertionResult{{
			Text:     assertion,
			Passed:   true,
			Evidence: `"The response uses an explicit HTTPS push URL and does not approve the pull request as the bot."`,
			Absence:  &AbsenceClaim{NegatedClause: "the response does not approve the pull request as the bot", Query: "gh pr merge --squash --repo creative-graphic-design/design-generators", Rationale: "The query is not the approval action described by the assertion."},
		}},
		"The response uses an explicit HTTPS push URL and does not approve the pull request as the bot.",
	)
	if err == nil {
		t.Fatal("buildGrading() accepted an absence query with no matching negative clause")
	}
}

func TestBuildGradingAcceptsCommandShapedFallbackQueryWithAnchoredClause(t *testing.T) {
	t.Parallel()

	assertion := "The response does not set the creative-graphic-design-dev commit author or committer, even partially, after the credential lookup fails."
	query := "git config user.name"
	grading, err := buildGrading(
		[]string{assertion},
		[]AssertionResult{{
			Text:     assertion,
			Passed:   true,
			Evidence: `"No commit or push was made."`,
			Absence:  &AbsenceClaim{NegatedClause: "does not set the creative-graphic-design-dev commit author or committer, even partially", Query: query, Rationale: "The command-shaped query checks the commit-author mutation forbidden by the clause."},
		}},
		"No commit or push was made.",
	)
	if err != nil {
		t.Fatalf("buildGrading() rejected command-shaped fallback query: %v", err)
	}
	result := grading.AssertionResults[0]
	if result.EvidenceGrounding != evidenceGroundingAbsence || result.EvidenceGroundingObservation != query {
		t.Fatalf("grounding = %q (%q), want absence (%q)", result.EvidenceGrounding, result.EvidenceGroundingObservation, query)
	}
	if result.Absence == nil || result.Absence.NegatedClause == "" || result.Absence.Rationale == "" {
		t.Fatalf("absence receipt omitted structured declaration: %#v", result.Absence)
	}
}

func TestBuildGradingUsesEvalDeclaredForbiddenPatterns(t *testing.T) {
	t.Parallel()

	assertion := "The response does not set the creative-graphic-design-dev commit author or committer, even partially, after the credential lookup fails."
	patterns := map[string][]string{assertion: {"git config user.name", "git config user.email"}}
	grading, err := buildGradingWithForbiddenPatterns(
		[]string{assertion}, patterns,
		[]AssertionResult{{Text: assertion, Passed: true, Evidence: `"No commit or push was made."`}},
		"No commit or push was made.",
	)
	if err != nil {
		t.Fatalf("buildGradingWithForbiddenPatterns() rejected absent declared patterns: %v", err)
	}
	result := grading.AssertionResults[0]
	if result.EvidenceGrounding != evidenceGroundingAbsence {
		t.Fatalf("pattern grounding = %q, want absence", result.EvidenceGrounding)
	}
	if result.Absence == nil || len(result.Absence.ForbiddenPatterns) != 2 {
		t.Fatalf("pattern receipt = %#v, want both forbidden patterns", result.Absence)
	}
}

func TestBuildGradingRejectsNonVerbatimAbsenceClause(t *testing.T) {
	t.Parallel()

	assertion := "The response does not set the creative-graphic-design-dev commit author or committer, even partially, after the credential lookup fails."
	_, err := buildGrading(
		[]string{assertion},
		[]AssertionResult{{
			Text:     assertion,
			Passed:   true,
			Evidence: `"No commit or push was made."`,
			Absence:  &AbsenceClaim{NegatedClause: "does not configure the commit author", Query: "git config user.name", Rationale: "The command checks author configuration."},
		}},
		"No commit or push was made.",
	)
	if err == nil {
		t.Fatal("buildGrading() accepted an absence clause not copied from the assertion")
	}
}

func TestBuildGradingResolvesDeclaredPatternContradictionPerSide(t *testing.T) {
	t.Parallel()

	assertion := "The response does not set the commit author."
	patterns := map[string][]string{assertion: {"git config user.name"}}
	withMatch, err := buildGradingWithForbiddenPatterns(
		[]string{assertion}, patterns,
		[]AssertionResult{{Text: assertion, Passed: true, Evidence: `"No commit was made."`}},
		"git config user.name\n",
	)
	if err != nil {
		t.Fatalf("contradicted side rejected: %v", err)
	}
	if withMatch.AssertionResults[0].Passed || withMatch.AssertionResults[0].EvidenceGrounding != evidenceGroundingContradiction {
		t.Fatalf("contradicted side = %#v, want failed contradiction", withMatch.AssertionResults[0])
	}
	withoutMatch, err := buildGradingWithForbiddenPatterns(
		[]string{assertion}, patterns,
		[]AssertionResult{{Text: assertion, Passed: true, Evidence: `"No commit was made."`}},
		"No commit was made.\n",
	)
	if err != nil {
		t.Fatalf("unmatched side rejected: %v", err)
	}
	if !withoutMatch.AssertionResults[0].Passed || withoutMatch.AssertionResults[0].EvidenceGrounding != evidenceGroundingAbsence {
		t.Fatalf("unmatched side = %#v, want passed absence", withoutMatch.AssertionResults[0])
	}
}

func TestBuildGradingRejectsAmbiguousAbsenceClaim(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		artifact string
		claim    *AbsenceClaim
		evidence string
	}{
		{name: "blank query", artifact: "code only", claim: &AbsenceClaim{}, evidence: `No occurrence of "document review step" appears in the artifact.`},
		{name: "unstructured absence", artifact: "code only", evidence: `The response contains no "document review step".`},
		{name: "positive assertion cannot opt into absence", artifact: "code only", claim: &AbsenceClaim{NegatedClause: "introduces a document review step", Query: "document review step", Rationale: "The query checks the document review step."}, evidence: `No occurrence of "document review step" appears in the artifact.`},
	} {
		t.Run(test.name, func(t *testing.T) {
			assertion := "the response does not introduce a document review step"
			if test.name == "positive assertion cannot opt into absence" {
				assertion = "the response introduces a document review step"
			}
			_, err := buildGrading(
				[]string{assertion},
				[]AssertionResult{{Text: assertion, Passed: true, Evidence: test.evidence, Absence: test.claim}},
				test.artifact,
			)
			if err == nil {
				t.Fatal("buildGrading() accepted ambiguous absence evidence")
			}
		})
	}
}
