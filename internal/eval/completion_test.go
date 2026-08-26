package eval

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

type completionEvalHarness struct {
	mu       sync.Mutex
	requests []harness.Request
}

func (*completionEvalHarness) Probe(_ context.Context, securities ...harness.SecurityResolution) (harness.Identity, error) {
	if len(securities) != 0 {
		return harness.Identity{}, errors.New("completion probe received security")
	}
	return harness.Identity{Agent: "fake", Version: "completion"}, nil
}

func (*completionEvalHarness) Capabilities() harness.Capabilities {
	return harness.Capabilities{Skills: true, Instructions: true}
}

func (*completionEvalHarness) ResolveSecurity(context.Context, harness.SecurityPolicy) (harness.SecurityResolution, error) {
	return harness.SecurityResolution{}, errors.New("completion resolved security")
}

func (h *completionEvalHarness) Run(_ context.Context, request harness.Request) (harness.Result, error) {
	h.mu.Lock()
	h.requests = append(h.requests, request)
	h.mu.Unlock()
	if request.Mode != harness.ModeCompletion || request.WorkDir != "" || request.Target != nil || !request.Security.Equal(harness.SecurityResolution{}) {
		return harness.Result{}, errors.New("completion request carried agentic state")
	}
	response := "candidate output"
	if len(request.OutputSchema) == 0 {
		if !strings.Contains(request.Prompt, "--- begin SKILL.md ---") {
			response = "baseline output"
		}
	} else if strings.Contains(string(request.OutputSchema), `"preferred"`) {
		preferred := "B"
		if strings.Contains(request.Prompt, `"A_response":"candidate output"`) {
			preferred = "A"
		} else if strings.Contains(request.Prompt, `"B_response":"candidate output"`) {
			preferred = "B"
		}
		response = `{"cases":[{"id":"1","trial":1,"preferred":"` + preferred + `","reason":"candidate output is more useful"}]}`
	} else {
		side := "A"
		if strings.Contains(request.Prompt, `"side":"B"`) {
			side = "B"
		}
		response = `{"cases":[{"id":"1","trial":1,"side":"` + side + `","assertion_results":[{"text":"The result is correct","passed":true,"evidence":"The response artifact is correct."}]}]}`
	}
	return harness.Result{Response: response, Transcript: []byte("completion transcript\n"), Duration: time.Millisecond, Attempts: harness.AttemptEvidence{AttemptCount: 1}}, nil
}

func TestCompletionEvaluationInlinesGuidanceAndSharedFixtures(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "---\nname: demo\ndescription: Demo skill\n---\n\nUse the guidance marker.\n")
	mustWrite(t, filepath.Join(root, "fixture.txt"), "fixture contents\n")
	mustWrite(t, filepath.Join(root, "evals", "evals.json"), `{"skill_name":"demo","evals":[{"id":1,"prompt":"do the task","expected_output":"a correct result","files":["fixture.txt"],"assertions":["The result is correct"]}]}`)
	suite, err := LoadSkillSuite(root)
	if err != nil {
		t.Fatal(err)
	}
	agent := &completionEvalHarness{}
	report, err := Run(context.Background(), suite, agent, Config{Mode: harness.ModeCompletion, Trials: 1, Jobs: 1, Timeout: time.Second, Workspace: filepath.Join(t.TempDir(), "workspace")})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("report = %#v", report)
	}
	agent.mu.Lock()
	requests := append([]harness.Request(nil), agent.requests...)
	agent.mu.Unlock()
	if len(requests) != 5 {
		t.Fatalf("completion requests = %d, want two runs, two graders, and one comparator", len(requests))
	}
	var withPrompt, withoutPrompt string
	for _, request := range requests {
		if len(request.OutputSchema) == 0 {
			if strings.Contains(request.Prompt, "--- begin SKILL.md ---") {
				withPrompt = request.Prompt
			} else {
				withoutPrompt = request.Prompt
			}
		}
	}
	if !strings.Contains(withPrompt, "Use the guidance marker") || !strings.Contains(withPrompt, "fixture contents") {
		t.Fatalf("with-guidance completion prompt = %q", withPrompt)
	}
	if strings.Contains(withoutPrompt, "Use the guidance marker") || !strings.Contains(withoutPrompt, "fixture contents") {
		t.Fatalf("without-guidance completion prompt lacks shared fixture or contains guidance = %q", withoutPrompt)
	}
	for _, path := range []string{filepath.Join(report.Workspace, "manifest.json"), filepath.Join(report.Workspace, "benchmark.json")} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var artifact struct {
			Mode     harness.Mode                `json:"mode"`
			Security *harness.SecurityResolution `json:"security"`
		}
		if err := json.Unmarshal(contents, &artifact); err != nil {
			t.Fatal(err)
		}
		if artifact.Mode != harness.ModeCompletion || artifact.Security != nil {
			t.Fatalf("completion provenance in %s = %#v", path, artifact)
		}
	}
}

func TestBuildCompletionPromptLeavesGuidanceOutOfBaseline(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	mustWrite(t, filepath.Join(root, "SKILL.md"), "skill guidance\n")
	mustWrite(t, filepath.Join(root, "fixture.txt"), "fixture\n")
	suite := Suite{Kind: harness.TargetSkill, Root: root, TargetPath: root}
	item := Case{Prompt: "task", Files: []string{"fixture.txt"}}
	with, err := buildCompletionPrompt(suite, item, true)
	if err != nil {
		t.Fatal(err)
	}
	without, err := buildCompletionPrompt(suite, item, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with, "skill guidance") || strings.Contains(without, "skill guidance") {
		t.Fatalf("guidance prompts = %q / %q", with, without)
	}
	if !strings.Contains(with, "fixture") || !strings.Contains(without, "fixture") {
		t.Fatalf("fixture was not inlined = %q / %q", with, without)
	}
}
