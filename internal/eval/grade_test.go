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

type batchingJudgeHarness struct {
	fakeHarness
	omitGraderKey     string
	omitComparatorKey string
}

func (h *batchingJudgeHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.runs++
	h.requests = append(h.requests, request)
	h.mu.Unlock()
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
			output.Cases = append(output.Cases, comparatorEntry{ID: input.ID, Trial: input.Trial, Preferred: "tie", Reason: "equivalent"})
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
			return []AssertionResult{{Text: "correct", Passed: true, Evidence: fmt.Sprintf(`Observed %q.`, observation)}}
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
	entries := make([]judgeEntry, 0, len(cases))
	comparisons := make([]comparatorEntry, 0, len(cases))
	results := make([]runResult, 0, len(cases)*2)
	for index, item := range cases {
		mapping := blindLabels(item.ID, 1, variantWithSkill, variantWithoutSkill)
		preferredVariant := variantWithSkill
		if index == len(cases)-1 {
			preferredVariant = variantWithoutSkill
		}
		grades := map[string][]AssertionResult{
			variantWithSkill:    {{Text: "correct", Passed: true, Evidence: `Observed "with_skill".`}},
			variantWithoutSkill: {{Text: "correct", Passed: true, Evidence: `Observed "without_skill".`}},
		}
		entries = append(entries, judgeEntry{
			ID: item.ID, Trial: 1,
			AAssertionResults: grades[mapping.A], BAssertionResults: grades[mapping.B],
		})
		comparisons = append(comparisons, comparatorEntry{ID: item.ID, Trial: 1, Preferred: preferredLabel(mapping, preferredVariant), Reason: "comparison"})
		for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
			runDir := filepath.Join(root, item.ID, variant)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: variant, Agent: harness.Result{Duration: time.Millisecond}})
		}
	}
	encoded, err := json.Marshal(judgeOutput{Cases: entries})
	if err != nil {
		t.Fatal(err)
	}
	compared, err := json.Marshal(comparatorOutput{Cases: comparisons})
	if err != nil {
		t.Fatal(err)
	}
	agent := &fakeHarness{judgeResponse: string(encoded), compareResponse: string(compared)}
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

func TestJudgeBatchingMatchesPreservedProductionShape(t *testing.T) {
	t.Parallel()

	contents, err := os.ReadFile(filepath.Join("testdata", "judge-batching-production-shape.json"))
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
	inputs := make([]judgeInput, 0, len(fixture.Cases))
	for _, item := range fixture.Cases {
		inputs = append(inputs, judgeInput{ID: item.ID, Trial: item.Trial, Assertions: []string{"correct"}, A: strings.Repeat("a", item.WithBytes), B: strings.Repeat("b", item.WithoutBytes)})
	}
	fullPrompt, err := structuredJudgePrompt(graderPrompt, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(fullPrompt) <= maxStructuredJudgePromptBytes {
		t.Fatalf("production-shaped aggregate = %d bytes, want over %d", len(fullPrompt), maxStructuredJudgePromptBytes)
	}
	batches, err := batchStructuredJudgeInputs("grader", graderPrompt, inputs, maxStructuredJudgePromptBytes, func(input judgeInput) (string, int) { return input.ID, input.Trial })
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) < 2 {
		t.Fatalf("batches = %d, want multiple", len(batches))
	}
	seen := map[string]int{}
	caseBatches := map[string]map[int]bool{}
	for batchIndex, batch := range batches {
		prompt, err := structuredJudgePrompt(graderPrompt, batch)
		if err != nil {
			t.Fatal(err)
		}
		if len(prompt) > maxStructuredJudgePromptBytes {
			t.Fatalf("batch %d = %d bytes, exceeds %d", batchIndex, len(prompt), maxStructuredJudgePromptBytes)
		}
		for _, input := range batch {
			key := caseTrialKey(input.ID, input.Trial)
			seen[key]++
			if caseBatches[input.ID] == nil {
				caseBatches[input.ID] = map[int]bool{}
			}
			caseBatches[input.ID][batchIndex] = true
		}
	}
	if len(seen) != len(inputs) {
		t.Fatalf("batched case-trials = %d, want %d", len(seen), len(inputs))
	}
	for key, count := range seen {
		if count != 1 {
			t.Fatalf("case-trial %q appears in %d batches", key, count)
		}
	}
	if len(caseBatches["case-c"]) < 2 {
		t.Fatal("fixture did not exercise the documented per-trial split for one case")
	}
}

func TestGradeRunsBatchesJudgesWithoutChangingBlindMappings(t *testing.T) {
	t.Parallel()

	suite, results := largeBatchingSuite(t)
	agent := &batchingJudgeHarness{}
	graded, _, _, _, _, err := gradeRuns(context.Background(), agent, suite, results, Config{Trials: 1, Timeout: time.Second}, t.TempDir())
	if err != nil {
		t.Fatalf("gradeRuns() error = %v", err)
	}
	if len(graded) != len(results) {
		t.Fatalf("graded runs = %d, want %d", len(graded), len(results))
	}
	graderRequests, comparatorRequests := 0, 0
	graderSeen := map[string]int{}
	comparatorSeen := map[string]int{}
	for _, request := range agent.requests {
		payload := request.Prompt[strings.LastIndex(request.Prompt, "\n\n")+2:]
		if strings.Contains(string(request.OutputSchema), `"preferred"`) {
			comparatorRequests++
			var inputs []comparatorInput
			if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
				t.Fatal(err)
			}
			for _, input := range inputs {
				key := caseTrialKey(input.ID, input.Trial)
				comparatorSeen[key]++
				mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
				artifacts := resultArtifacts(results, input.ID, input.Trial)
				if input.A != artifacts[mapping.A] || input.B != artifacts[mapping.B] {
					t.Fatalf("comparator batch changed blind mapping for %s", key)
				}
			}
			continue
		}
		graderRequests++
		var inputs []judgeInput
		if err := json.Unmarshal([]byte(payload), &inputs); err != nil {
			t.Fatal(err)
		}
		for _, input := range inputs {
			key := caseTrialKey(input.ID, input.Trial)
			graderSeen[key]++
			mapping := blindLabels(input.ID, input.Trial, variantWithSkill, variantWithoutSkill)
			artifacts := resultArtifacts(results, input.ID, input.Trial)
			if input.A != artifacts[mapping.A] || input.B != artifacts[mapping.B] {
				t.Fatalf("batch changed blind mapping for %s", key)
			}
		}
	}
	if graderRequests < 2 || comparatorRequests < 2 {
		t.Fatalf("judge requests grader=%d comparator=%d, want separate multiple batches", graderRequests, comparatorRequests)
	}
	for stage, seen := range map[string]map[string]int{"grader": graderSeen, "comparator": comparatorSeen} {
		if len(seen) != len(suite.Cases) {
			t.Fatalf("%s covered %d case-trials, want %d", stage, len(seen), len(suite.Cases))
		}
		for key, count := range seen {
			if count != 1 {
				t.Fatalf("%s case-trial %q appeared %d times", stage, key, count)
			}
		}
	}
}

func TestGradeRunsRejectsIncompleteBatchOutput(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		agent *batchingJudgeHarness
		stage string
	}{
		{name: "grader", agent: &batchingJudgeHarness{omitGraderKey: caseTrialKey("three", 1)}, stage: "grader batch"},
		{name: "comparator", agent: &batchingJudgeHarness{omitComparatorKey: caseTrialKey("three", 1)}, stage: "comparator batch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			suite, results := largeBatchingSuite(t)
			_, _, _, _, _, err := gradeRuns(context.Background(), test.agent, suite, results, Config{Trials: 1, Timeout: time.Second}, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), test.stage) {
				t.Fatalf("gradeRuns() error = %v, want incomplete %s", err, test.stage)
			}
		})
	}
}

func TestJudgeBatchingRejectsSingleOversizedCaseTrial(t *testing.T) {
	t.Parallel()

	input := judgeInput{ID: "giant", Trial: 7, Assertions: []string{"correct"}, A: strings.Repeat("a", maxStructuredJudgePromptBytes), B: "b"}
	_, err := batchStructuredJudgeInputs("grader", graderPrompt, []judgeInput{input}, maxStructuredJudgePromptBytes, func(input judgeInput) (string, int) { return input.ID, input.Trial })
	if err == nil {
		t.Fatal("batchStructuredJudgeInputs() accepted an oversized case-trial")
	}
	for _, want := range []string{`case "giant"`, "trial 7", "bytes", "budget"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("oversized error %q lacks %q", err, want)
		}
	}
}

func largeBatchingSuite(t *testing.T) (Suite, []runResult) {
	t.Helper()
	cases := []Case{{ID: "one", Assertions: []string{"correct"}}, {ID: "two", Assertions: []string{"correct"}}, {ID: "three", Assertions: []string{"correct"}}}
	results := make([]runResult, 0, len(cases)*2)
	for _, item := range cases {
		for _, variant := range []string{variantWithSkill, variantWithoutSkill} {
			runDir := filepath.Join(t.TempDir(), item.ID, variant)
			if err := os.MkdirAll(runDir, 0o755); err != nil {
				t.Fatal(err)
			}
			artifact := variant + "-" + item.ID + "\n" + strings.Repeat("x", 200_000)
			results = append(results, runResult{Case: item, Trial: 1, Variant: variant, RunDir: runDir, Artifact: artifact})
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
