package receipt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
)

func TestWriteTimingAggregatesExhaustedAttemptDurations(t *testing.T) {
	t.Parallel()

	attempts := harness.AttemptEvidence{
		AttemptCount: 3,
		AttemptErrors: []harness.AttemptError{
			{Attempt: 1, Error: "disconnect one", Timestamp: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), DurationMS: 11, StdoutBytes: 101, StderrBytes: 11},
			{Attempt: 2, Error: "disconnect two", Timestamp: time.Date(2026, 8, 17, 12, 0, 1, 0, time.UTC), DurationMS: 22, StdoutBytes: 202, StderrBytes: 22},
			{Attempt: 3, Error: "disconnect three", Timestamp: time.Date(2026, 8, 17, 12, 0, 2, 0, time.UTC), DurationMS: 33, StdoutBytes: 303, StderrBytes: 33},
		},
	}
	path := filepath.Join(t.TempDir(), "timing.json")
	if err := WriteTiming(path, harness.Usage{}, 0, attempts); err != nil {
		t.Fatalf("WriteTiming() error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var artifact struct {
		SchemaVersion string                 `json:"schema_version"`
		DurationMS    int64                  `json:"duration_ms"`
		AttemptCount  int                    `json:"attempt_count"`
		AttemptErrors []harness.AttemptError `json:"attempt_errors"`
	}
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact.SchemaVersion != "1" || artifact.DurationMS != 66 || artifact.AttemptCount != 3 || len(artifact.AttemptErrors) != 3 {
		t.Fatalf("timing artifact = %#v, want version 1 and three attempts totaling 66 ms", artifact)
	}
}

func TestWriteTimingPreservesSuccessfulAttemptFields(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "timing.json")
	if err := WriteTiming(path, harness.Usage{InputTokens: 3, OutputTokens: 2}, 15*time.Millisecond, harness.AttemptEvidence{}); err != nil {
		t.Fatalf("WriteTiming() error = %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var artifact map[string]any
	if err := json.Unmarshal(contents, &artifact); err != nil {
		t.Fatal(err)
	}
	if artifact["total_tokens"] != float64(5) || artifact["duration_ms"] != float64(15) || artifact["attempt_count"] != float64(1) {
		t.Fatalf("successful timing fields changed: %s", contents)
	}
	if _, exists := artifact["attempt_errors"]; exists {
		t.Fatalf("successful timing unexpectedly contains attempt_errors: %s", contents)
	}
}

func TestWriteTimingRejectsArtifactOutsideSchema(t *testing.T) {
	t.Parallel()

	attempts := harness.AttemptEvidence{
		AttemptCount:  1,
		AttemptErrors: []harness.AttemptError{{Attempt: 1, Error: "missing measurements"}},
	}
	if err := WriteTiming(filepath.Join(t.TempDir(), "timing.json"), harness.Usage{}, 0, attempts); err == nil {
		t.Fatal("WriteTiming() accepted an attempt error outside timing.schema.json")
	}
}
