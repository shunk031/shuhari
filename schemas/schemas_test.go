package schemas

import "testing"

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
