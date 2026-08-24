package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValidatesSkillFrontmatterAndDirectoryOwnership(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		contents string
		wantErr  string
	}{
		{name: "missing frontmatter", contents: "# Demo\n", wantErr: "must start"},
		{name: "unclosed frontmatter", contents: "---\nname: demo\n", wantErr: "not closed"},
		{name: "missing description", contents: "---\nname: demo\ndescription: \n---\n", wantErr: "requires name and description"},
		{name: "wrong directory", contents: "---\nname: other\ndescription: Demo\n---\n", wantErr: "does not match directory"},
		{name: "invalid yaml", contents: "---\nname: [\ndescription: Demo\n---\n", wantErr: "decode"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(test.contents), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(root); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Load() error = %v, want %q", err, test.wantErr)
			}
		})
	}

	valid := "---\nname: demo\ndescription: Demo skill\n---\n\n# Demo\n"
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	metadata, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "demo" || metadata.Description != "Demo skill" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestLoadReportsMissingSkillFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "read SKILL.md") {
		t.Fatalf("Load() error = %v, want missing file", err)
	}
}
