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
			Absence:  &AbsenceClaim{Query: "document review step"},
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
			Absence:  &AbsenceClaim{Query: query},
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

func TestBuildGradingRejectsUnmatchedMixedAssertionAbsenceClaim(t *testing.T) {
	t.Parallel()

	assertion := "The response uses an explicit HTTPS push URL and does not approve the pull request as the bot."
	_, err := buildGrading(
		[]string{assertion},
		[]AssertionResult{{
			Text:     assertion,
			Passed:   true,
			Evidence: `"The response uses an explicit HTTPS push URL and does not approve the pull request as the bot."`,
			Absence:  &AbsenceClaim{Query: "gh pr merge --squash --repo creative-graphic-design/design-generators"},
		}},
		"The response uses an explicit HTTPS push URL and does not approve the pull request as the bot.",
	)
	if err == nil {
		t.Fatal("buildGrading() accepted an absence query with no matching negative clause")
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
		{name: "query present", artifact: "the artifact contains document review step", claim: &AbsenceClaim{Query: "document review step"}, evidence: `No occurrence of "document review step" appears in the artifact.`},
		{name: "blank query", artifact: "code only", claim: &AbsenceClaim{}, evidence: `No occurrence of "document review step" appears in the artifact.`},
		{name: "unstructured absence", artifact: "code only", evidence: `The response contains no "document review step".`},
		{name: "positive assertion cannot opt into absence", artifact: "code only", claim: &AbsenceClaim{Query: "document review step"}, evidence: `No occurrence of "document review step" appears in the artifact.`},
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
