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
