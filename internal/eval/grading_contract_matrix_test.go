package eval

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type gradingMatrixAssertionType string
type gradingMatrixVerdict string
type gradingMatrixAttachment string
type gradingMatrixMechanical string
type gradingMatrixAbsenceMode string

const (
	matrixPositive gradingMatrixAssertionType = "positive"
	matrixNegative gradingMatrixAssertionType = "negative"
	matrixMixed    gradingMatrixAssertionType = "mixed"

	matrixPass gradingMatrixVerdict = "pass"
	matrixFail gradingMatrixVerdict = "fail"

	matrixEvidenceRefs gradingMatrixAttachment = "evidence refs"
	matrixAbsence      gradingMatrixAttachment = "absence object"
	matrixBoth         gradingMatrixAttachment = "both"
	matrixNeither      gradingMatrixAttachment = "neither"

	matrixNotApplicable gradingMatrixMechanical = "not-applicable"
	matrixNoMatch       gradingMatrixMechanical = "no-match"
	matrixMatch         gradingMatrixMechanical = "match"
	matrixContradiction gradingMatrixMechanical = "contradiction"

	matrixFallback gradingMatrixAbsenceMode = "fallback"
	matrixDeclared gradingMatrixAbsenceMode = "declared-patterns"
)

type gradingMatrixCell struct {
	name        string
	mode        gradingMatrixAbsenceMode
	assertion   gradingMatrixAssertionType
	verdict     gradingMatrixVerdict
	attachment  gradingMatrixAttachment
	mechanical  gradingMatrixMechanical
	wantError   bool
	wantPassed  bool
	wantGround  string
	wantObserve string
}

func TestBuildGradingContractMatrix(t *testing.T) {
	for _, cell := range gradingContractMatrix() {
		cell := cell
		t.Run(cell.name, func(t *testing.T) {
			assertion := matrixAssertion(cell.assertion)
			artifact := matrixArtifact(cell.mechanical)
			root := t.TempDir()
			writeMatrixArtifact(t, root, artifact)

			result := AssertionResult{
				Text:     assertion,
				Passed:   cell.verdict == matrixPass,
				Evidence: "judge evidence",
			}
			if cell.attachment == matrixEvidenceRefs || cell.attachment == matrixBoth {
				result.Evidence = "positive evidence"
				result.EvidenceReferences = []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 1}}
			}
			if cell.attachment == matrixAbsence || cell.attachment == matrixBoth {
				result.Absence = matrixAbsenceClaim(cell.assertion)
			}

			patterns := map[string][]string(nil)
			if cell.mode == matrixDeclared {
				patterns = map[string][]string{assertion: {"forbidden command"}}
			}
			grading, err := buildAgentGradingWithForbiddenPatterns([]string{assertion}, patterns, []AssertionResult{result}, root, artifact)
			if cell.wantError {
				if err == nil || !errors.Is(err, errInvalidGrading) {
					t.Fatalf("buildAgentGrading() error = %v, want invalid grading", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildAgentGrading() rejected valid matrix cell: %v", err)
			}
			got := grading.AssertionResults[0]
			if got.Passed != cell.wantPassed {
				t.Fatalf("passed = %v, want %v; result=%#v", got.Passed, cell.wantPassed, got)
			}
			if got.EvidenceGrounding != cell.wantGround {
				t.Fatalf("grounding = %q, want %q; result=%#v", got.EvidenceGrounding, cell.wantGround, got)
			}
			if cell.wantObserve != "" && got.EvidenceGroundingObservation != cell.wantObserve {
				t.Fatalf("grounding observation = %q, want %q", got.EvidenceGroundingObservation, cell.wantObserve)
			}
		})
	}
	t.Logf("grading contract matrix cells: %d", len(gradingContractMatrix()))
}

func TestRetainedGradingCorpus(t *testing.T) {
	const mixed = "The response uses an explicit HTTPS push URL and does not approve the pull request as the bot."
	const negative = "The response does not set the creative-graphic-design-dev commit author or committer, even partially, after the credential lookup fails."
	corpus := []struct {
		name       string
		assertion  string
		result     AssertionResult
		artifact   string
		wantError  bool
		wantPass   bool
		wantGround string
	}{
		{
			name:      "iteration-27 mixed absence clause omitted",
			assertion: mixed,
			result:    AssertionResult{Text: mixed, Passed: true, Evidence: "git push --set-upstream origin \"$BRANCH\"", EvidenceReferences: []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 1}}, Absence: &AbsenceClaim{Query: "gh pr review --approve", Rationale: "The query checks the approval command."}},
			artifact:  "git push --set-upstream origin \"$BRANCH\"\n",
			wantError: true,
		},
		{
			name:       "iteration-28 hallucinated negative absence resolves contradiction",
			assertion:  "The response does not use gh auth switch as the runtime account-selection mechanism.",
			result:     AssertionResult{Text: "The response does not use gh auth switch as the runtime account-selection mechanism.", Passed: true, Evidence: "No forbidden command was found.", Absence: &AbsenceClaim{NegatedClause: "does not use gh auth switch as the runtime account-selection mechanism", Query: "gh auth switch", Rationale: "The query checks the forbidden command."}},
			artifact:   "gh auth switch --hostname github.com --user example\n",
			wantPass:   false,
			wantGround: evidenceGroundingContradiction,
		},
		{
			name:      "iteration-30 malformed declared absence fails closed",
			assertion: negative,
			result:    AssertionResult{Text: negative, Passed: true, Evidence: "No commit or push was made.", Absence: &AbsenceClaim{Query: "git config user.name"}},
			artifact:  "No commit or push was made.\n",
			wantError: true,
		},
		{
			name:      "iteration-31 generic positive evidence fails closed",
			assertion: "The response verifies push permission before writing.",
			result:    AssertionResult{Text: "The response verifies push permission before writing.", Passed: true, Evidence: "the requested permission check was performed", EvidenceReferences: []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 1}}},
			artifact:  "gh api repos/example/repo --jq .permissions.push\n",
			wantError: true,
		},
		{
			name:       "iteration-34 failed mixed result continues",
			assertion:  mixed,
			result:     AssertionResult{Text: mixed, Passed: false, Evidence: "git push --set-upstream origin \"$BRANCH\"", EvidenceReferences: []EvidenceReference{{Path: "response.md", StartLine: 1, EndLine: 1}}, Absence: &AbsenceClaim{NegatedClause: "does not approve the pull request as the bot.", Query: "gh pr review --approve", Rationale: "The query checks the approval command."}},
			artifact:   "git push --set-upstream origin \"$BRANCH\"\n",
			wantPass:   false,
			wantGround: evidenceGroundingNotApplicable,
		},
	}
	for _, fixture := range corpus {
		fixture := fixture
		t.Run(fixture.name, func(t *testing.T) {
			root := t.TempDir()
			writeMatrixArtifact(t, root, fixture.artifact)
			grading, err := buildAgentGrading([]string{fixture.assertion}, []AssertionResult{fixture.result}, root, fixture.artifact)
			if fixture.wantError {
				if err == nil || !errors.Is(err, errInvalidGrading) {
					t.Fatalf("error = %v, want invalid grading", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("retained fixture rejected: %v", err)
			}
			got := grading.AssertionResults[0]
			if got.Passed != fixture.wantPass || got.EvidenceGrounding != fixture.wantGround {
				t.Fatalf("result = %#v, want passed=%v grounding=%q", got, fixture.wantPass, fixture.wantGround)
			}
		})
	}
}

func TestComparatorContractMatrix(t *testing.T) {
	input := []comparatorInput{{ID: "case", Trial: 1}}
	cases := []struct {
		name    string
		output  comparatorOutput
		wantErr bool
	}{
		{name: "A resolves", output: comparatorOutput{Cases: []comparatorEntry{{ID: "case", Trial: 1, Preferred: "A", Reason: "A is materially more useful"}}}},
		{name: "B resolves", output: comparatorOutput{Cases: []comparatorEntry{{ID: "case", Trial: 1, Preferred: "B", Reason: "B is materially more useful"}}}},
		{name: "tie resolves", output: comparatorOutput{Cases: []comparatorEntry{{ID: "case", Trial: 1, Preferred: "tie", Reason: "Neither output is materially better"}}}},
		{name: "nonblank weak reason still resolves", output: comparatorOutput{Cases: []comparatorEntry{{ID: "case", Trial: 1, Preferred: "tie", Reason: "x"}}}},
		{name: "invalid preference fails closed", output: comparatorOutput{Cases: []comparatorEntry{{ID: "case", Trial: 1, Preferred: "C", Reason: "reason"}}}, wantErr: true},
		{name: "blank reason fails closed", output: comparatorOutput{Cases: []comparatorEntry{{ID: "case", Trial: 1, Preferred: "A", Reason: " "}}}, wantErr: true},
		{name: "missing case fails closed", output: comparatorOutput{}, wantErr: true},
		{name: "duplicate case fails closed", output: comparatorOutput{Cases: []comparatorEntry{{ID: "case", Trial: 1, Preferred: "A", Reason: "one"}, {ID: "case", Trial: 1, Preferred: "B", Reason: "two"}}}, wantErr: true},
		{name: "wrong case fails closed", output: comparatorOutput{Cases: []comparatorEntry{{ID: "other", Trial: 1, Preferred: "A", Reason: "reason"}}}, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateComparatorEntries(test.output, input)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateComparatorEntries() error = %v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func gradingContractMatrix() []gradingMatrixCell {
	var cells []gradingMatrixCell
	attachments := []gradingMatrixAttachment{matrixEvidenceRefs, matrixAbsence, matrixBoth, matrixNeither}
	for _, verdict := range []gradingMatrixVerdict{matrixPass, matrixFail} {
		for _, attachment := range attachments {
			cells = append(cells, gradingMatrixCell{
				name: matrixCellName(matrixFallback, matrixPositive, verdict, attachment, matrixNotApplicable),
				mode: matrixFallback, assertion: matrixPositive, verdict: verdict, attachment: attachment, mechanical: matrixNotApplicable,
				wantError:  (verdict == matrixPass && attachment != matrixEvidenceRefs) || (verdict == matrixFail && (attachment == matrixAbsence || attachment == matrixBoth)),
				wantPassed: verdict == matrixPass,
				wantGround: matrixPositiveGrounding(verdict, attachment),
			})
		}
	}
	for _, mode := range []gradingMatrixAbsenceMode{matrixFallback, matrixDeclared} {
		types := []gradingMatrixAssertionType{matrixNegative, matrixMixed}
		if mode == matrixDeclared {
			types = []gradingMatrixAssertionType{matrixNegative}
		}
		for _, assertion := range types {
			for _, verdict := range []gradingMatrixVerdict{matrixPass, matrixFail} {
				for _, attachment := range attachments {
					mechanicals := []gradingMatrixMechanical{matrixNotApplicable}
					if mode == matrixDeclared || attachment == matrixAbsence || attachment == matrixBoth {
						mechanicals = []gradingMatrixMechanical{matrixNoMatch}
						if verdict == matrixPass {
							mechanicals = append(mechanicals, matrixMatch)
						} else {
							mechanicals = append(mechanicals, matrixContradiction)
						}
					}
					for _, mechanical := range mechanicals {
						cell := gradingMatrixCell{
							name: matrixCellName(mode, assertion, verdict, attachment, mechanical), mode: mode, assertion: assertion, verdict: verdict, attachment: attachment, mechanical: mechanical,
						}
						setMatrixExpected(&cell)
						cells = append(cells, cell)
					}
				}
			}
		}
	}
	return cells
}

func setMatrixExpected(cell *gradingMatrixCell) {
	cell.wantPassed = cell.verdict == matrixPass
	if cell.assertion == matrixNegative || cell.assertion == matrixMixed {
		if cell.verdict == matrixFail {
			cell.wantGround = evidenceGroundingNotApplicable
			cell.wantError = false
			return
		}
		validAttachment := cell.attachment == matrixAbsence || cell.attachment == matrixBoth
		if cell.assertion == matrixMixed {
			validAttachment = cell.attachment == matrixBoth
		}
		if cell.mode == matrixDeclared {
			validAttachment = true
		}
		if !validAttachment {
			cell.wantError = true
			return
		}
		if cell.mechanical == matrixMatch {
			cell.wantPassed = false
			cell.wantGround = evidenceGroundingContradiction
			cell.wantObserve = "forbidden command"
			return
		}
		cell.wantGround = evidenceGroundingAbsence
		cell.wantObserve = "forbidden command"
		return
	}
	cell.wantError = true
}

func matrixPositiveGrounding(verdict gradingMatrixVerdict, attachment gradingMatrixAttachment) string {
	if verdict == matrixFail && (attachment == matrixEvidenceRefs || attachment == matrixNeither) {
		return evidenceGroundingNotApplicable
	}
	if verdict == matrixPass && attachment == matrixEvidenceRefs {
		return evidenceGroundingStrong
	}
	return ""
}

func matrixCellName(mode gradingMatrixAbsenceMode, assertion gradingMatrixAssertionType, verdict gradingMatrixVerdict, attachment gradingMatrixAttachment, mechanical gradingMatrixMechanical) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s", mode, assertion, verdict, attachment, mechanical)
}

func matrixAssertion(assertion gradingMatrixAssertionType) string {
	switch assertion {
	case matrixPositive:
		return "the response uses the required command"
	case matrixNegative:
		return "the response does not use the forbidden command"
	case matrixMixed:
		return "the response uses the required command and does not use the forbidden command"
	default:
		panic("unknown matrix assertion type")
	}
}

func matrixAbsenceClaim(assertion gradingMatrixAssertionType) *AbsenceClaim {
	negated := matrixAssertion(assertion)
	if assertion == matrixMixed {
		negated = "does not use the forbidden command"
	}
	return &AbsenceClaim{NegatedClause: negated, Query: "forbidden command", Rationale: "The query checks the forbidden command."}
}

func matrixArtifact(mechanical gradingMatrixMechanical) string {
	if mechanical == matrixMatch || mechanical == matrixContradiction {
		return "positive evidence\nforbidden command\n"
	}
	return "positive evidence\n"
}

func writeMatrixArtifact(t *testing.T, root, artifact string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "response.md"), []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}
}
