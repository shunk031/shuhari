package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckedInSchemasAreValidJSON(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"evals", "workspace", "grading", "comparison", "benchmark", "trigger"} {
		contents, err := os.ReadFile(filepath.Join("..", "..", "schemas", name+".schema.json"))
		if err != nil {
			t.Fatalf("read %s schema: %v", name, err)
		}
		var document map[string]any
		if err := json.Unmarshal(contents, &document); err != nil {
			t.Fatalf("decode %s schema: %v", name, err)
		}
		if document["$schema"] == nil || document["$id"] == nil {
			t.Fatalf("%s schema lacks identity", name)
		}
		if name == "workspace" || name == "benchmark" || name == "trigger" {
			properties, ok := document["properties"].(map[string]any)
			if !ok {
				t.Fatalf("%s schema lacks properties", name)
			}
			version, ok := properties["schema_version"].(map[string]any)
			if !ok || version["const"] != "2" {
				t.Fatalf("%s schema version = %#v, want 2", name, version["const"])
			}
			if _, ok := properties["security"]; !ok {
				t.Fatalf("%s schema lacks security contract", name)
			}
		}
	}
}
