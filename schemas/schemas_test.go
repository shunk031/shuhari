package schemas

import (
	"testing"
	"time"
)

func TestValidateRejectsArtifactOutsideCheckedInSchema(t *testing.T) {
	artifact := map[string]any{
		"schema_version": "2", "security": map[string]any{}, "run_summary": map[string]any{}, "assertion_analysis": []any{}, "unexpected": true,
	}
	if err := Validate("benchmark", artifact); err == nil {
		t.Fatal("Validate() accepted an artifact outside benchmark.schema.json")
	}
}

func TestTimingSchemaRequiresMeasuredAttemptErrors(t *testing.T) {
	artifact := map[string]any{
		"schema_version": "1", "total_tokens": 0, "duration_ms": 25, "attempt_count": 2,
		"attempt_errors": []any{map[string]any{"attempt": 1, "error": "transport failed", "timestamp": time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "duration_ms": 25, "stdout_bytes": 120, "stderr_bytes": 40}},
	}
	if err := Validate("timing", artifact); err != nil {
		t.Fatalf("Validate(timing) rejected measured retry: %v", err)
	}
	delete(artifact["attempt_errors"].([]any)[0].(map[string]any), "timestamp")
	if err := Validate("timing", artifact); err == nil {
		t.Fatal("Validate(timing) accepted an attempt error without a timestamp")
	}
}

func TestTriggerApplicationSchemaRejectsContradictoryVerdict(t *testing.T) {
	artifact := map[string]any{"schema_version": "1", "target_read": true, "verdict": "declined", "applied": false, "evidence": "The agent declined the skill."}
	if err := Validate("trigger-application", artifact); err != nil {
		t.Fatalf("Validate(trigger-application) rejected a consistent artifact: %v", err)
	}
	artifact["applied"] = true
	if err := Validate("trigger-application", artifact); err == nil {
		t.Fatal("Validate(trigger-application) accepted a contradictory artifact")
	}
}

func TestTriggerSchemasKeepCompletionProvenanceSeparate(t *testing.T) {
	completion := map[string]any{
		"schema_version": "3",
		"mode":           "completion",
		"security":       nil,
		"decision_rule":  "majority",
		"passed":         true,
		"target_invoked": map[string]any{"case": []any{true}},
	}
	if err := Validate("trigger", completion); err != nil {
		t.Fatalf("Validate(trigger) rejected completion provenance: %v", err)
	}
	completion["target_applied"] = map[string]any{"case": []any{true}}
	if err := Validate("trigger", completion); err == nil {
		t.Fatal("Validate(trigger) accepted completion target_applied provenance")
	}

	application := map[string]any{
		"schema_version": "1",
		"mode":           "completion",
		"target_invoked": true,
		"verdict":        "invoked",
		"evidence":       "The model decided to invoke the skill.",
		"decision":       map[string]any{"invoke": true, "reason": "The prompt matches."},
	}
	if err := Validate("trigger-application", application); err != nil {
		t.Fatalf("Validate(trigger-application) rejected completion provenance: %v", err)
	}
	application["applied"] = false
	if err := Validate("trigger-application", application); err == nil {
		t.Fatal("Validate(trigger-application) accepted completion applied provenance")
	}
}

func TestReferenceGradingSchemaUsesFreeFormEvidence(t *testing.T) {
	grading := map[string]any{
		"expectations": []any{
			map[string]any{"text": "The response is useful.", "passed": true, "evidence": "A generic judge observation."},
			map[string]any{"text": "The response is safe.", "passed": false, "evidence": "The judge found the requirement unmet."},
		},
		"summary": map[string]any{"passed": 1, "failed": 1, "total": 2, "pass_rate": 0.5},
	}
	if err := Validate("grading", grading); err != nil {
		t.Fatalf("Validate(grading) rejected reference-shaped grading: %v", err)
	}
	grading["assertion_results"] = grading["expectations"]
	delete(grading, "expectations")
	if err := Validate("grading", grading); err == nil {
		t.Fatal("Validate(grading) accepted the removed assertion_results shape")
	}
}

func TestEvalSchemaRejectsGroundingExtensions(t *testing.T) {
	evals := map[string]any{
		"skill_name": "demo",
		"evals":      []any{map[string]any{"id": "one", "prompt": "do it", "expected_output": "done", "assertions": []any{"The result is useful."}}},
	}
	if err := Validate("evals", evals); err != nil {
		t.Fatalf("Validate(evals) rejected reference-shaped evals: %v", err)
	}
	evals["evals"].([]any)[0].(map[string]any)["required_actions"] = []any{"file_change"}
	if err := Validate("evals", evals); err == nil {
		t.Fatal("Validate(evals) accepted removed required_actions")
	}
}
