package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

type perCaseJudgeHarness struct {
	fakeHarness
	omitGraderKey     string
	omitComparatorKey string
	rejectOverBytes   int
	preferredVariants map[string]string
}

func (h *perCaseJudgeHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.runs++
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if h.rejectOverBytes > 0 && len(request.Prompt) > h.rejectOverBytes {
		return harness.Result{}, fmt.Errorf("input_too_large: prompt is %d bytes; limit is %d bytes", len(request.Prompt), h.rejectOverBytes)
	}
	payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
	if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		var inputs []comparatorInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			return harness.Result{}, err
		}
		output := comparatorOutput{}
		for _, input := range inputs {
			if caseTrialKey(input.ID, input.Trial) == h.omitComparatorKey {
				continue
			}
			preferred := "tie"
			if variant := h.preferredVariants[input.ID]; variant != "" {
				mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
				preferred = preferredLabel(mapping, variant)
			}
			output.Cases = append(output.Cases, comparatorEntry{ID: input.ID, Trial: input.Trial, Preferred: preferred, Reason: "comparison"})
		}
		encoded, _ := json.Marshal(output)
		return harness.Result{Response: string(encoded)}, nil
	}
	var inputs []judgeInput
	if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
		return harness.Result{}, err
	}
	output := judgeOutput{}
	for _, input := range inputs {
		if caseTrialKey(input.ID, input.Trial) == h.omitGraderKey {
			continue
		}
		results := func(artifact string) []AssertionResult {
			observation := strings.SplitN(artifact, "\n", 2)[0]
			results := make([]AssertionResult, 0, len(input.Assertions))
			for _, assertion := range input.Assertions {
				results = append(results, AssertionResult{Text: assertion, Passed: true, Evidence: fmt.Sprintf(`Observed %q.`, observation)})
			}
			return results
		}
		output.Cases = append(output.Cases, judgeEntry{ID: input.ID, Trial: input.Trial, AAssertionResults: results(input.A), BAssertionResults: results(input.B)})
	}
	encoded, _ := json.Marshal(output)
	return harness.Result{Response: string(encoded)}, nil
}

func TestGradeRunsAllowsMinorityBaselinePreference(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cases := []Case{
		{ID: "one", ExpectedOutput: "correct"},
		{ID: "two", ExpectedOutput: "correct"},
		{ID: "three", ExpectedOutput: "correct"},
	}
	results := make([]runResult, 0, len(cases)*2)
	for _, item := range cases {
		for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
			runDir := filepath.Join(root, item.ID, variant)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: variant, Agent: harness.Result{Duration: time.Millisecond}})
		}
	}
	agent := &perCaseJudgeHarness{preferredVariants: map[string]string{
		"one":   variantWithSkill,
		"two":   variantWithSkill,
		"three": variantWithoutSkill,
	}}
	_, candidateWins, baselineWins, reasons, _, err := gradeRuns(context.Background(), agent, Suite{Kind: harness.TargetSkill, Cases: cases}, results, Config{Trials: 1, Timeout: time.Second}, root)
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	if candidateWins != 2 || baselineWins != 1 {
		t.Fatalf("wins = %d/%d, want 2/1", candidateWins, baselineWins)
	}
	if len(reasons) != 0 {
		t.Fatalf("minority baseline preference produced failure reasons: %v", reasons)
	}
}

func TestBuildGradingRejectsBlankOrUnsupportedEvidence(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		evidence string
	}{
		{name: "blank", evidence: "   "},
		{name: "not quoted", evidence: "the output is correct"},
		{name: "quote not in artifact", evidence: `Observed "invented".`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := buildGrading([]string{"correct"}, []AssertionResult{{Text: "correct", Passed: true, Evidence: test.evidence}}, "actual output")
			if err == nil {
				t.Fatal("buildGrading() accepted invalid evidence")
			}
		})
	}
	grading, err := buildGrading([]string{"correct"}, []AssertionResult{{Text: "correct", Passed: true, Evidence: `Observed "actual output".`}}, "actual output")
	if err != nil || grading.Summary.Passed != 1 {
		t.Fatalf("valid evidence rejected: grading=%#v err=%v", grading, err)
	}
	if !evidenceQuotesArtifact(`Excerpt: "first\nsecond"`, "first\nsecond") {
		t.Fatal("escaped newline in quoted evidence was not matched to the artifact")
	}
	if evidenceQuotesArtifact("Observed `actual output`.", "actual output") {
		t.Fatal("backticks were accepted as quotation marks")
	}
}

func TestBuildGradingValidatesPreservedProductionEvidence(t *testing.T) {
	t.Parallel()

	fixtureBytes, err := os.ReadFile(filepath.Join("testdata", "grader-evidence-backticks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Source   string `json:"source"`
		Artifact string `json:"artifact"`
		Evidence string `json:"evidence"`
		Valid    bool   `json:"valid"`
	}
	if err := json.Unmarshal(fixtureBytes, &fixtures); err != nil {
		t.Fatal(err)
	}

	for _, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Source, func(t *testing.T) {
			grading, err := buildGrading(
				[]string{"artifact observation is grounded"},
				[]AssertionResult{{Text: "artifact observation is grounded", Passed: true, Evidence: fixture.Evidence}},
				fixture.Artifact,
			)
			if fixture.Valid && err != nil {
				t.Fatalf("buildGrading() rejected preserved production evidence: %v", err)
			}
			if !fixture.Valid && err == nil {
				t.Fatal("buildGrading() accepted preserved evidence with no verbatim artifact observation")
			}
			if fixture.Valid && grading.Summary.Passed != 1 {
				t.Fatalf("passed assertions = %d, want 1", grading.Summary.Passed)
			}
		})
	}
}

func TestComparatorInputIncludesOriginalTask(t *testing.T) {
	t.Parallel()

	input := comparatorInput{Prompt: "original task", ExpectedOutput: "expected", Assertions: []string{"check"}}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"original task", "expected", "check"} {
		if !strings.Contains(string(encoded), value) {
			t.Fatalf("comparator input omits %q: %s", value, encoded)
		}
	}
}

func TestJudgeSchemasAreValidJSON(t *testing.T) {
	t.Parallel()

	for name, schema := range map[string][]byte{"grader": graderSchema(), "comparator": comparatorSchema()} {
		if !json.Valid(schema) {
			t.Fatalf("%s schema is not valid JSON: %s", name, schema)
		}
	}
}

func TestPerCaseJudgePromptsFitPreservedProductionSizeShape(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("testdata", "judge-production-size-shape.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			ID           string `json:"id"`
			Trial        int    `json:"trial"`
			WithBytes    int    `json:"with_bytes"`
			WithoutBytes int    `json:"without_bytes"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatal(err)
	}
	allInputs := make([]judgeInput, 0, len(fixture.Cases))
	byCase := map[string][]judgeInput{}
	for _, item := range fixture.Cases {
		input := judgeInput{ID: item.ID, Trial: item.Trial, Assertions: []string{"correct"}, A: strings.Repeat("a", item.WithBytes), B: strings.Repeat("b", item.WithoutBytes)}
		allInputs = append(allInputs, input)
		byCase[item.ID] = append(byCase[item.ID], input)
	}
	fullPrompt, err := structuredJudgePrompt(graderPrompt, allInputs)
	if err != nil {
		t.Fatal(err)
	}
	const codexInputLimit = 1_048_576
	if len(fullPrompt) <= codexInputLimit {
		t.Fatalf("production-shaped aggregate = %d bytes, want over %d", len(fullPrompt), codexInputLimit)
	}
	for id, inputs := range byCase {
		prompt, err := structuredJudgePrompt(graderPrompt, inputs)
		if err != nil {
			t.Fatal(err)
		}
		if len(prompt) > codexInputLimit {
			t.Fatalf("case %q prompt = %d bytes, exceeds %d", id, len(prompt), codexInputLimit)
		}
	}
}

func TestGradeRunsJudgesEachCaseSeparately(t *testing.T) {
	t.Parallel()

	suite, results := perCaseJudgeSuite(t, 2)
	agent := &perCaseJudgeHarness{}
	graded, _, _, _, _, err := gradeRuns(context.Background(), agent, suite, results, Config{Trials: 2, Timeout: time.Second}, t.TempDir())
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	if len(graded) != len(results) {
		t.Fatalf("graded runs = %d, want %d", len(graded), len(results))
	}
	seen := map[string]map[string]int{"grader": {}, "comparator": {}}
	for _, request := range agent.requests {
		payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
		if strings.Contains(string(request.OutputSchema), `"preferred"`) {
			var inputs []comparatorInput
			if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
				t.Fatal(err)
			}
			if len(inputs) != 2 {
				t.Fatalf("comparator case trials = %d, want 2", len(inputs))
			}
			id := inputs[0].ID
			seen["comparator"][id]++
			for _, input := range inputs {
				if input.ID != id {
					t.Fatalf("comparator mixed cases %q and %q", id, input.ID)
				}
				mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
				artifacts := resultArtifacts(results, input.ID, input.Trial)
				if input.A != artifacts[mapping.A] || input.B != artifacts[mapping.B] {
					t.Fatalf("comparator changed blind mapping for %s", caseTrialKey(input.ID, input.Trial))
				}
			}
			continue
		}
		var inputs []judgeInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			t.Fatal(err)
		}
		if len(inputs) != 2 {
			t.Fatalf("grader case trials = %d, want 2", len(inputs))
		}
		id := inputs[0].ID
		seen["grader"][id]++
		for _, input := range inputs {
			if input.ID != id {
				t.Fatalf("grader mixed cases %q and %q", id, input.ID)
			}
			mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
			artifacts := resultArtifacts(results, input.ID, input.Trial)
			if input.A != artifacts[mapping.A] || input.B != artifacts[mapping.B] {
				t.Fatalf("grader changed blind mapping for %s", caseTrialKey(input.ID, input.Trial))
			}
		}
	}
	for stage, cases := range seen {
		if len(cases) != len(suite.Cases) {
			t.Fatalf("%s covered %d cases, want %d", stage, len(cases), len(suite.Cases))
		}
		for id, count := range cases {
			if count != 1 {
				t.Fatalf("%s case %q was judged %d times", stage, id, count)
			}
		}
	}
}

func TestGradeRunsRejectsIncompletePerCaseOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		agent *perCaseJudgeHarness
		stage string
	}{
		{name: "grader", agent: &perCaseJudgeHarness{omitGraderKey: caseTrialKey("three", 2)}, stage: `grader case "three"`},
		{name: "comparator", agent: &perCaseJudgeHarness{omitComparatorKey: caseTrialKey("three", 2)}, stage: `comparator case "three"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, results := perCaseJudgeSuite(t, 2)
			_, _, _, _, _, err := gradeRuns(context.Background(), test.agent, suite, results, Config{Trials: 2, Timeout: time.Second}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.stage) {
				t.Fatalf("gradeRuns() error = %v, want incomplete %s", err, test.stage)
			}
		})
	}
}

func TestGradeRunsReportsOversizedCasePrompt(t *testing.T) {
	t.Parallel()

	item := Case{ID: "oversized", Assertions: []string{"correct"}}
	results := make([]runResult, 0, 2)
	for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
		runDir := filepath.Join(t.TempDir(), variant)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: strings.Repeat(variant, 1_000)})
	}
	_, _, _, _, _, err := gradeRuns(context.Background(), &perCaseJudgeHarness{rejectOverBytes: 1_000}, Suite{Kind: harness.TargetSkill, Cases: []Case{item}}, results, Config{Trials: 1, Timeout: time.Second}, t.TempDir())
	if err == nil {
		t.Fatal("gradeRuns() accepted an oversized case prompt")
	}
	for _, want := range []string{`grader case "oversized"`, "prompt is", "bytes", "input_too_large"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("oversized error %q lacks %q", err, want)
		}
	}
}

func perCaseJudgeSuite(t *testing.T, trials int) (Suite, []runResult) {
	t.Helper()
	cases := []Case{{ID: "one", Assertions: []string{"correct"}}, {ID: "two", Assertions: []string{"correct"}}, {ID: "three", Assertions: []string{"correct"}}}
	results := make([]runResult, 0, len(cases)*trials*2)
	for _, item := range cases {
		for trial := 1; trial <= trials; trial++ {
			for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
				runDir := filepath.Join(t.TempDir(), item.ID, variant)
				if err := os.MkdirAll(runDir, 0o755); err != nil {
					t.Fatal(err)
				}
				artifact := fmt.Sprintf("%s-%s-%d", variant, item.ID, trial)
				results = append(results, runResult{Case: item, Trial: trial, Variant: variant, RunDir: runDir, Artifact: artifact})
			}
		}
	}
	return Suite{Kind: harness.TargetSkill, Cases: cases}, results
}

func resultArtifacts(results []runResult, id string, trial int) map[string]string {
	artifacts := map[string]string{}
	for _, result := range results {
		if result.Case.ID == id && result.Trial == trial {
			artifacts[result.Variant] = result.Artifact
		}
	}
	return artifacts
}
