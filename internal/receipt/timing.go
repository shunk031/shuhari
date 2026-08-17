package receipt

import (
	"encoding/json"
	"os"
	"time"

	"github.com/shunk031/shuhari/internal/harness"
	contracts "github.com/shunk031/shuhari/schemas"
)

const timingSchemaVersion = "1"

type timingArtifact struct {
	SchemaVersion string `json:"schema_version"`
	TotalTokens   int64  `json:"total_tokens"`
	DurationMS    int64  `json:"duration_ms"`
	harness.AttemptEvidence
}

func WriteTiming(path string, usage harness.Usage, duration time.Duration, attempts harness.AttemptEvidence) error {
	if attempts.AttemptCount == 0 {
		attempts.AttemptCount = 1
	}
	if duration <= 0 {
		for _, attempt := range attempts.AttemptErrors {
			duration += time.Duration(attempt.DurationMS) * time.Millisecond
		}
	}
	artifact := timingArtifact{
		SchemaVersion:   timingSchemaVersion,
		TotalTokens:     usage.TotalTokens(),
		DurationMS:      duration.Milliseconds(),
		AttemptEvidence: attempts,
	}
	if err := contracts.Validate("timing", artifact); err != nil {
		return err
	}
	contents, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	contents = append(contents, '\n')
	return os.WriteFile(path, contents, 0o644)
}
