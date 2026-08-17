package trigger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCasePassUsesMajorityForPositive(t *testing.T) {
	t.Parallel()

	if !casePass([]bool{true, false, true}, true, false) {
		t.Fatal("positive 2/3 majority did not pass")
	}
	if casePass([]bool{true, false, true}, true, true) {
		t.Fatal("strict positive accepted a miss")
	}
}

func TestCasePassUsesMajorityForNegativeControls(t *testing.T) {
	t.Parallel()

	if !casePass([]bool{false, true, false}, false, false) {
		t.Fatal("negative control with one read did not pass by majority")
	}
	if casePass([]bool{true, true, false}, false, false) {
		t.Fatal("negative control accepted a majority of reads")
	}
	if casePass([]bool{false, true, false}, false, true) {
		t.Fatal("strict negative control accepted one read")
	}
}

func TestDecisionRuleReflectsStrictPolicy(t *testing.T) {
	t.Parallel()

	if got := decisionRule(false); got != "majority" {
		t.Fatalf("default decision rule = %q, want majority", got)
	}
	if got := decisionRule(true); got != "strict" {
		t.Fatalf("strict decision rule = %q, want strict", got)
	}
}

func TestLoadSuiteRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: demo\ndescription: Demo\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contents := `{"skill_name":"demo","cases":[{"id":1,"prompt":"yes","should_trigger":true},{"id":2,"prompt":"no","should_trigger":false}]} {}`
	if err := os.WriteFile(filepath.Join(root, "evals", "triggers.json"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuite(root, ""); err == nil {
		t.Fatal("LoadSuite() accepted a trailing JSON value")
	}
}

func TestDigestSuiteIncludesExternalCasesFile(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	casesPath := filepath.Join(t.TempDir(), "triggers.json")
	if err := os.WriteFile(casesPath, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	suite := Suite{SkillPath: root, CasesPath: casesPath}
	first, err := digestSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(casesPath, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := digestSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("digest did not change with external cases file")
	}
}

func TestLoadSuiteRejectsMismatchedSkillFrontmatter(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: another-name\ndescription: Demo\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contents := `{"skill_name":"demo","cases":[{"id":1,"prompt":"yes","should_trigger":true},{"id":2,"prompt":"no","should_trigger":false}]}`
	if err := os.WriteFile(filepath.Join(root, "evals", "triggers.json"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuite(root, ""); err == nil {
		t.Fatal("LoadSuite() accepted a mismatched SKILL.md name")
	}
}

func TestDigestSuiteIncludesRelativeFilenames(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(root, "references", "a.md")
	if err := os.WriteFile(firstPath, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	suite := Suite{SkillPath: root, CasesPath: firstPath}
	first, err := digestSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(root, "references", "b.md")
	if err := os.Rename(firstPath, secondPath); err != nil {
		t.Fatal(err)
	}
	suite.CasesPath = secondPath
	second, err := digestSuite(suite)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("digest did not change when a skill file was renamed")
	}
}

func TestApplyPolicySeparatesMeasurementFromAcceptance(t *testing.T) {
	t.Parallel()

	suite := Suite{Cases: []Case{{ID: "positive", ShouldTrigger: true}, {ID: "negative", ShouldTrigger: false}}}
	measurement := Measurement{Applications: map[string][]bool{"positive": {true, false, true}, "negative": {false, true, false}}}
	reasons := ApplyPolicy(suite, measurement, Policy{Trials: 3})
	if len(reasons) != 0 {
		t.Fatalf("reasons = %v, want positive and negative majority pass", reasons)
	}

	measurement.Applications["negative"] = []bool{true, true, false}
	reasons = ApplyPolicy(suite, measurement, Policy{Trials: 3})
	if len(reasons) != 1 || !strings.Contains(reasons[0], "negative") {
		t.Fatalf("reasons = %v, want systematic negative over-trigger failure", reasons)
	}

	measurement.Applications["negative"] = []bool{false, true, false}
	reasons = ApplyPolicy(suite, measurement, Policy{Trials: 3, StrictAllTrials: true})
	if len(reasons) != 2 || !strings.Contains(strings.Join(reasons, " "), "positive") || !strings.Contains(strings.Join(reasons, " "), "negative") {
		t.Fatalf("strict reasons = %v, want positive miss and negative read failures", reasons)
	}
}

func TestLoadSuiteRejectsNormalizedIDCollision(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("---\nname: demo\ndescription: Demo\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	contents := `{"skill_name":"demo","cases":[{"id":"a/b","prompt":"yes","should_trigger":true},{"id":"a b","prompt":"no","should_trigger":false}]}`
	if err := os.WriteFile(filepath.Join(root, "evals", "triggers.json"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSuite(root, ""); err == nil {
		t.Fatal("LoadSuite() accepted colliding workspace IDs")
	}
}
