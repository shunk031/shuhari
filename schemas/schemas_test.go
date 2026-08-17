package schemas

import (
	"testing"
	"time"
)

func TestValidateRejectsArtifactOutsideCheckedInSchema(t *testing.T) {
	t.Parallel()

	artifact := map[string]any{
		"schema_version": "2",
		"security": map[string]any{
			"sandbox_level":       "isolated",
			"network_access":      "denied",
			"credential_boundary": "enforced",
			"adapter": map[string]any{
				"name":          "fake",
				"native_mode":   "fake-isolated",
				"policy_digest": "x",
			},
		},
		"run_summary":        map[string]any{},
		"assertion_analysis": []any{},
	}
	if err := Validate("benchmark", artifact); err == nil {
		t.Fatal("Validate() accepted an artifact outside benchmark.schema.json")
	}
}

func TestTimingSchemaRequiresMeasuredAttemptErrors(t *testing.T) {
	t.Parallel()

	artifact := map[string]any{
		"schema_version": "1",
		"total_tokens":   0,
		"duration_ms":    25,
		"attempt_count":  2,
		"attempt_errors": []any{
			map[string]any{
				"attempt":      1,
				"error":        "transport failed",
				"timestamp":    time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
				"duration_ms":  25,
				"stdout_bytes": 120,
				"stderr_bytes": 40,
			},
		},
	}
	if err := Validate("timing", artifact); err != nil {
		t.Fatalf("Validate() rejected measured timing artifact: %v", err)
	}
	delete(artifact["attempt_errors"].([]any)[0].(map[string]any), "timestamp")
	if err := Validate("timing", artifact); err == nil {
		t.Fatal("Validate() accepted an attempt error without a timestamp")
	}
}

func TestTriggerApplicationSchemaRejectsContradictoryVerdict(t *testing.T) {
	t.Parallel()

	artifact := map[string]any{
		"schema_version": "1",
		"target_read":    true,
		"verdict":        "declined",
		"applied":        false,
		"evidence":       "The agent explicitly declined the skill.",
	}
	if err := Validate("trigger-application", artifact); err != nil {
		t.Fatalf("Validate() rejected a consistent trigger application artifact: %v", err)
	}
	artifact["applied"] = true
	if err := Validate("trigger-application", artifact); err == nil {
		t.Fatal("Validate() accepted a declined verdict marked as applied")
	}
}

func TestAbsenceSchemasAcceptDeclaredPatternsAndFallbackClaims(t *testing.T) {
	t.Parallel()

	evals := map[string]any{
		"skill_name": "demo",
		"evals": []any{map[string]any{
			"id":              "absence",
			"prompt":          "check the workspace",
			"expected_output": "the check completes",
			"assertions": []any{map[string]any{
				"text":               "The response does not set the commit author.",
				"forbidden_patterns": []any{"git config user.name"},
			}},
		}},
	}
	if err := Validate("evals", evals); err != nil {
		t.Fatalf("Validate(evals) rejected declared absence patterns: %v", err)
	}

	grading := map[string]any{
		"assertion_results": []any{
			map[string]any{
				"text":     "The response does not set the commit author.",
				"passed":   true,
				"evidence": `Observed "No commit was made."`,
				"absence": map[string]any{
					"negated_clause": "does not set the commit author",
					"query":          "git config user.name",
					"rationale":      "The query checks author configuration.",
				},
			},
			map[string]any{
				"text":     "The response does not set the commit author.",
				"passed":   true,
				"evidence": `Observed "No commit was made."`,
				"absence": map[string]any{
					"forbidden_patterns": []any{"git config user.name"},
				},
			},
		},
		"summary": map[string]any{"passed": 2, "failed": 0, "total": 2, "pass_rate": 1.0},
	}
	if err := Validate("grading", grading); err != nil {
		t.Fatalf("Validate(grading) rejected fallback/pattern absence receipts: %v", err)
	}
}
