package trigger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

type applicationVerdictHarness struct {
	mu             sync.Mutex
	judgeError     error
	judgePrompts   []string
	judgeVerdicts  map[string]string
	judgeEvidence  map[string]string
	transcriptRoot string
}

func (*applicationVerdictHarness) Probe(context.Context, ...harness.SecurityResolution) (harness.Identity, error) {
	return harness.Identity{Agent: "fake", Version: "1"}, nil
}

func (*applicationVerdictHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, TriggerEvidence: true}
}

func (*applicationVerdictHarness) ResolveSecurity(_ context.Context, policy harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return fakeTriggerSecurityResolution(policy), nil
}

func (h *applicationVerdictHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	if request.OutputSchema != nil {
		h.mu.Lock()
		h.judgePrompts = append(h.judgePrompts, request.Prompt)
		h.mu.Unlock()
		if h.judgeError != nil {
			return harness.Result{}, h.judgeError
		}
		for marker, verdict := range h.judgeVerdicts {
			if strings.Contains(request.Prompt, marker) {
				encoded, _ := json.Marshal(map[string]string{"verdict": verdict, "evidence": h.judgeEvidence[marker]})
				return harness.Result{Response: string(encoded), Duration: time.Millisecond}, nil
			}
		}
		return harness.Result{}, errors.New("judge prompt did not match a fixture")
	}
	name := request.Prompt
	contents, err := os.ReadFile(filepath.Join(h.transcriptRoot, name+".jsonl"))
	if err != nil {
		return harness.Result{}, err
	}
	return harness.Result{
		Response:   "done",
		Transcript: contents,
		TargetRead: name != "no-read",
		Duration:   time.Millisecond,
	}, nil
}

func TestDecodeApplicationJudgeOutputRejectsInvalidResponses(t *testing.T) {
	t.Parallel()

	for _, response := range []string{
		`not json`,
		`{"verdict":"applied","evidence":"grounded"} {}`,
		`{"verdict":"unknown","evidence":"grounded"}`,
		`{"verdict":"declined","evidence":" "}`,
	} {
		if _, err := decodeApplicationJudgeOutput(response); err == nil {
			t.Fatalf("decodeApplicationJudgeOutput(%q) succeeded", response)
		}
	}
}

func TestClassifyApplicationBoundsLargeTranscriptWithDigest(t *testing.T) {
	t.Parallel()

	suite := newApplicationSuite(t, []Case{{ID: "large", Prompt: "large", ShouldTrigger: true}})
	agent := &applicationVerdictHarness{
		judgeVerdicts: map[string]string{"return exactly": "applied"},
		judgeEvidence: map[string]string{"return exactly": "The compact transcript shows the skill-specific action."},
	}
	transcript := []byte(fmt.Sprintf("{\"type\":\"item.completed\",\"item\":{\"type\":\"command_execution\",\"command\":\"cat .agents/skills/demo-skill/SKILL.md\",\"aggregated_output\":%q,\"exit_code\":0,\"status\":\"completed\"}}\n", strings.Repeat("oversized command output ", 100000)))
	_, err := classifyApplication(context.Background(), suite, agent, Config{Timeout: time.Second}, fakeTriggerSecurityResolution(harness.SecurityPolicy{Level: harness.SandboxReadOnly}), t.TempDir(), suite.Cases[0], harness.Result{TargetRead: true, Transcript: transcript})
	if err != nil {
		t.Fatalf("classifyApplication() error = %v", err)
	}
	if len(agent.judgePrompts) != 1 {
		t.Fatalf("judge prompts = %d, want 1", len(agent.judgePrompts))
	}
	prompt := agent.judgePrompts[0]
	if len(prompt) >= 1<<20 {
		t.Fatalf("bounded application judge prompt = %d bytes, want below 1 MiB", len(prompt))
	}
	for _, marker := range []string{"transcript_sha256", "transcript_bytes", "transcript_truncated"} {
		if !strings.Contains(prompt, marker) {
			t.Fatalf("bounded application judge prompt omitted %q", marker)
		}
	}
	if strings.Contains(prompt, strings.Repeat("oversized command output ", 100)) {
		t.Fatal("application judge prompt inlined the oversized command output")
	}
}

func TestRunClassifiesApplicationInsteadOfConsultation(t *testing.T) {
	t.Parallel()

	suite := newApplicationSuite(t, []Case{
		{ID: "decline-a", Prompt: "read-then-decline-a", ShouldTrigger: false},
		{ID: "decline-b", Prompt: "read-then-decline-b", ShouldTrigger: false},
		{ID: "no-read", Prompt: "no-read", ShouldTrigger: false},
		{ID: "apply", Prompt: "read-then-apply", ShouldTrigger: true},
	})
	agent := &applicationVerdictHarness{
		transcriptRoot: "testdata",
		judgeVerdicts: map[string]string{
			"outside the demo service scope":  "declined",
			"intentionally out of scope":      "declined",
			"required service-check workflow": "applied",
		},
		judgeEvidence: map[string]string{
			"outside the demo service scope":  "The agent explicitly declined the skill and used only the local fixture.",
			"intentionally out of scope":      "The agent explicitly declined the skill and kept the operation local.",
			"required service-check workflow": "The agent followed the skill-specific service-check workflow.",
		},
	}
	report, err := Run(context.Background(), suite, agent, Config{
		Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !report.Passed {
		t.Fatalf("report = %#v, want pass", report)
	}
	if len(agent.judgePrompts) != 3 {
		t.Fatalf("judge calls = %d, want 3 consulted trials", len(agent.judgePrompts))
	}

	contents, err := os.ReadFile(filepath.Join(report.Workspace, "trigger.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		SchemaVersion string            `json:"schema_version"`
		TargetRead    map[string][]bool `json:"target_read"`
		TargetApplied map[string][]bool `json:"target_applied"`
	}
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.SchemaVersion != "3" {
		t.Fatalf("schema_version = %q, want 3", artifact.SchemaVersion)
	}
	if !artifact.TargetRead["decline-a"][0] || artifact.TargetApplied["decline-a"][0] {
		t.Fatalf("decline-a evidence = read %v applied %v, want true false", artifact.TargetRead["decline-a"], artifact.TargetApplied["decline-a"])
	}
	if artifact.TargetRead["no-read"][0] || artifact.TargetApplied["no-read"][0] {
		t.Fatalf("no-read evidence = read %v applied %v, want false false", artifact.TargetRead["no-read"], artifact.TargetApplied["no-read"])
	}
	if !artifact.TargetApplied["apply"][0] {
		t.Fatalf("apply evidence = %v, want true", artifact.TargetApplied["apply"])
	}
	assertApplicationArtifact(t, report.Workspace, "decline-a", "declined", false)
	assertApplicationArtifact(t, report.Workspace, "no-read", "not_consulted", false)
	assertApplicationArtifact(t, report.Workspace, "apply", "applied", true)
}

func TestRunFailsClosedOnAmbiguousApplication(t *testing.T) {
	t.Parallel()

	suite := newApplicationSuite(t, []Case{
		{ID: "apply", Prompt: "read-then-apply", ShouldTrigger: true},
		{ID: "ambiguous", Prompt: "read-then-decline-a", ShouldTrigger: false},
	})
	agent := &applicationVerdictHarness{
		transcriptRoot: "testdata",
		judgeVerdicts: map[string]string{
			"required service-check workflow": "applied",
			"outside the demo service scope":  "ambiguous",
		},
		judgeEvidence: map[string]string{
			"required service-check workflow": "The agent followed the skill-specific workflow.",
			"outside the demo service scope":  "The transcript contains conflicting signals.",
		},
	}
	report, err := Run(context.Background(), suite, agent, Config{
		Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"),
	})
	if err == nil || !strings.Contains(err.Error(), "application verdict is ambiguous") {
		t.Fatalf("Run() error = %v, want ambiguous application refusal", err)
	}
	assertApplicationArtifact(t, report.Workspace, "ambiguous", "ambiguous", false)
}

func TestRunRecordsApplicationJudgeTransportExhaustion(t *testing.T) {
	t.Parallel()

	suite := newApplicationSuite(t, []Case{
		{ID: "apply", Prompt: "read-then-apply", ShouldTrigger: true},
		{ID: "no-read", Prompt: "no-read", ShouldTrigger: false},
	})
	attemptErrors := make([]harness.AttemptError, 0, 3)
	for attempt := 1; attempt <= 3; attempt++ {
		attemptErrors = append(attemptErrors, harness.AttemptError{
			Attempt: attempt, Error: "stream disconnected before completion", Timestamp: time.Now().UTC(), DurationMS: 10, StdoutBytes: 12, StderrBytes: 34,
		})
	}
	agent := &applicationVerdictHarness{
		transcriptRoot: "testdata",
		judgeError: &harness.RetryError{
			Cause:    errors.New("stream disconnected before completion"),
			Attempts: harness.AttemptEvidence{AttemptCount: 3, AttemptErrors: attemptErrors},
		},
	}
	report, err := Run(context.Background(), suite, agent, Config{
		Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace"),
	})
	if err == nil || !strings.Contains(err.Error(), "run application judge") {
		t.Fatalf("Run() error = %v, want application judge transport error", err)
	}
	contents, readErr := os.ReadFile(filepath.Join(report.Workspace, "case-apply", "trial-1", "application-timing.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(contents), `"attempt_count": 3`) || strings.Count(string(contents), "stream disconnected before completion") != 3 {
		t.Fatalf("application timing lacks exhausted attempts: %s", contents)
	}
}

func newApplicationSuite(t *testing.T, cases []Case) Suite {
	t.Helper()
	root := filepath.Join(t.TempDir(), "demo-skill")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: demo-skill\ndescription: Check the demo service\n---\n\nWhen the demo service is in scope, run `demo-service-check --required`.\n"
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	casesPath := filepath.Join(root, "evals", "triggers.json")
	encoded, err := json.Marshal(map[string]any{"skill_name": "demo-skill", "cases": cases})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casesPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
	return Suite{SkillName: "demo-skill", SkillPath: root, CasesPath: casesPath, Cases: cases}
}

func assertApplicationArtifact(t *testing.T, workspace, caseID, verdict string, applied bool) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(workspace, "case-"+caseID, "trial-1", "application.json"))
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		Verdict string `json:"verdict"`
		Applied bool   `json:"applied"`
	}
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.Verdict != verdict || artifact.Applied != applied {
		t.Fatalf("application artifact = %#v, want verdict %q applied %v", artifact, verdict, applied)
	}
}

func TestApplicationPromptDeclinesWithoutAnExplicitStatement(t *testing.T) {
	t.Parallel()

	// A negative control passes when the agent does not act on the skill. An
	// agent that correctly ignores an irrelevant skill does not announce that
	// it is ignoring it; it just does the work. Requiring the announcement sent
	// that outcome to `ambiguous`, which is a hard error rather than a verdict,
	// so a correct negative control could not pass.
	// The prompt is hard-wrapped, so compare against a whitespace-collapsed copy.
	flat := strings.Join(strings.Fields(applicationPrompt), " ")
	if strings.Contains(flat, "explicitly determines that the skill is out of scope") {
		t.Fatalf("application prompt still requires an explicit out-of-scope statement for declined:\n%s", applicationPrompt)
	}
	if !strings.Contains(flat, "The agent does not have to say it is declining.") {
		t.Fatalf("application prompt no longer states that silence is declined:\n%s", applicationPrompt)
	}
}

func TestApplicationPromptJudgesOnlyTheTargetSkill(t *testing.T) {
	t.Parallel()

	// The judge receives skill_name. Without being told to confine itself to
	// that skill, it weighed whether the agent accepted or declined an
	// unrelated skill that happened to be present, and returned `ambiguous`
	// for a run that had in fact applied the target skill.
	flat := strings.Join(strings.Fields(applicationPrompt), " ")
	if !strings.Contains(flat, "Judge only the skill named in `skill_name`.") {
		t.Fatalf("application prompt does not confine the judge to the target skill:\n%s", applicationPrompt)
	}
}

func TestApplicationPromptDoesNotRequireEveryInstruction(t *testing.T) {
	t.Parallel()

	// `applied` needs one skill-specific instruction, not all of them. A judge
	// that treats an unfollowed instruction as disqualifying returned
	// `declined` for a run whose own evidence said the agent had used the
	// skill's guidance — the skill also told it to review the result with a
	// different skill, which this task had no reason to do.
	flat := strings.Join(strings.Fields(applicationPrompt), " ")
	if !strings.Contains(flat, "One is enough.") {
		t.Fatalf("application prompt does not state that one instruction suffices:\n%s", applicationPrompt)
	}
	if !strings.Contains(flat, "Instructions the agent did not follow do not undo that") {
		t.Fatalf("application prompt does not say unfollowed instructions leave `applied` standing:\n%s", applicationPrompt)
	}
}

func TestApplicationPromptSeparatesSkillsNamedInsideTheTarget(t *testing.T) {
	t.Parallel()

	// The target skill's own body may tell the agent to use another skill by
	// name. Reading that name in the body is not a reason to judge the agent
	// against the other skill.
	flat := strings.Join(strings.Fields(applicationPrompt), " ")
	if !strings.Contains(flat, "including any the target skill names in its own body") {
		t.Fatalf("application prompt does not cover skills named inside the target:\n%s", applicationPrompt)
	}
}
