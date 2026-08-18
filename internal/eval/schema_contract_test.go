package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/shunk031/shuhari/internal/harness"
)

func TestSchemaPrimitiveDecodersRejectAmbiguousInputs(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		data string
	}{
		{name: "empty string case id", data: `""`},
		{name: "object case id", data: `{}`},
		{name: "malformed case id", data: `not-json`},
		{name: "fractional case id", data: `1.5`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var id caseID
			if err := id.UnmarshalJSON([]byte(test.data)); err == nil {
				t.Fatalf("caseID.UnmarshalJSON() accepted %s", test.name)
			}
		})
	}
	var id caseID
	if err := id.UnmarshalJSON([]byte(`"case"`)); err != nil || id != "case" {
		t.Fatalf("string case id = %q, err=%v", id, err)
	}
	if err := id.UnmarshalJSON([]byte(`42`)); err != nil || id != "42" {
		t.Fatalf("numeric case id = %q, err=%v", id, err)
	}

	for _, test := range []struct {
		name string
		data string
	}{
		{name: "unknown assertion field", data: `{"text":"claim","unknown":true}`},
		{name: "trailing assertion value", data: `{"text":"claim"}{}`},
		{name: "malformed assertion suffix", data: `{"text":"claim"} trailing`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var assertion rawAssertion
			if err := assertion.UnmarshalJSON([]byte(test.data)); err == nil {
				t.Fatalf("rawAssertion.UnmarshalJSON() accepted %s", test.name)
			}
		})
	}
	var assertion rawAssertion
	if err := assertion.UnmarshalJSON([]byte(`"claim"`)); err != nil || assertion.Text != "claim" || len(assertion.ForbiddenPatterns) != 0 {
		t.Fatalf("string assertion = %#v, err=%v", assertion, err)
	}
	if err := assertion.UnmarshalJSON([]byte(`{"text":"does not change","forbidden_patterns":["git config user.name"]}`)); err != nil || len(assertion.ForbiddenPatterns) != 1 {
		t.Fatalf("object assertion = %#v, err=%v", assertion, err)
	}
}

func TestDecodeStrictJSONRejectsUnknownAndTrailingContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	tests := []struct {
		name string
		data string
	}{
		{name: "missing file", data: ""},
		{name: "invalid json", data: "{"},
		{name: "unknown field", data: `{"name":"demo","extra":true}`},
		{name: "trailing value", data: `{"name":"demo"}{}`},
		{name: "malformed trailing value", data: `{"name":"demo"} trailing`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.name+".json")
			if test.data != "" {
				mustWrite(t, path, test.data)
			}
			var destination struct {
				Name string `json:"name"`
			}
			if err := decodeStrictJSON(path, &destination); err == nil {
				t.Fatalf("decodeStrictJSON() accepted %s", test.name)
			}
		})
	}
	valid := filepath.Join(root, "valid.json")
	mustWrite(t, valid, `{"name":"demo"}`)
	var destination struct {
		Name string `json:"name"`
	}
	if err := decodeStrictJSON(valid, &destination); err != nil || destination.Name != "demo" {
		t.Fatalf("decodeStrictJSON() valid result = %#v, err=%v", destination, err)
	}
}

func TestValidateCasesRejectsInvalidContractShapes(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "fixture-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := rawCase{ID: "one", Prompt: "prompt", ExpectedOutput: "expected"}
	for _, test := range []struct {
		name string
		raw  []rawCase
	}{
		{name: "no cases", raw: nil},
		{name: "empty id", raw: []rawCase{{Prompt: "prompt", ExpectedOutput: "expected"}}},
		{name: "duplicate id", raw: []rawCase{base, base}},
		{name: "blank prompt", raw: []rawCase{{ID: "one", Prompt: " ", ExpectedOutput: "expected"}}},
		{name: "blank expected output", raw: []rawCase{{ID: "one", Prompt: "prompt", ExpectedOutput: " "}}},
		{name: "empty assertion", raw: []rawCase{{ID: "one", Prompt: "prompt", ExpectedOutput: "expected", Assertions: []rawAssertion{{Text: " "}}}}},
		{name: "forbidden patterns on positive", raw: []rawCase{{ID: "one", Prompt: "prompt", ExpectedOutput: "expected", Assertions: []rawAssertion{{Text: "sets the author", ForbiddenPatterns: []string{"git config user.name"}}}}}},
		{name: "empty forbidden pattern", raw: []rawCase{{ID: "one", Prompt: "prompt", ExpectedOutput: "expected", Assertions: []rawAssertion{{Text: "does not set the author", ForbiddenPatterns: []string{" "}}}}}},
		{name: "duplicate forbidden pattern", raw: []rawCase{{ID: "one", Prompt: "prompt", ExpectedOutput: "expected", Assertions: []rawAssertion{{Text: "does not set the author", ForbiddenPatterns: []string{"git config user.name", "git config user.name"}}}}}},
		{name: "unsupported action", raw: []rawCase{{ID: "one", Prompt: "prompt", ExpectedOutput: "expected", RequiredActions: []harness.Action{"unsupported"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateCases(root, test.raw); err == nil {
				t.Fatalf("validateCases() accepted %s", test.name)
			}
		})
	}
	for _, relative := range []string{"", "/absolute", "../escape", "missing.txt", "fixture-dir"} {
		if err := validateFixture(root, relative); err == nil {
			t.Fatalf("validateFixture() accepted %q", relative)
		}
	}
	if err := validateFixture(root, "fixture.txt"); err != nil {
		t.Fatalf("validateFixture() rejected a regular file: %v", err)
	}
}

func TestValidateCasesAcceptsAllSupportedActionsAndPatterns(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	valid := []rawCase{{
		ID: "one", Prompt: "prompt", ExpectedOutput: "expected",
		Files:           []string{"fixture.txt"},
		Assertions:      []rawAssertion{{Text: "does not set the author", ForbiddenPatterns: []string{"git config user.name"}}},
		RequiredActions: []harness.Action{harness.ActionWebSearch, harness.ActionGitHubSearch, harness.ActionFileChange},
	}}
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := validateCases(root, valid)
	if err != nil || len(cases) != 1 || len(cases[0].ForbiddenPatterns["does not set the author"]) != 1 {
		t.Fatalf("validateCases() valid result = %#v, err=%v", cases, err)
	}
}
