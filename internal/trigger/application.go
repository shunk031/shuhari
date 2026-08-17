package trigger

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shunk031/shuhari/internal/harness"
	"github.com/shunk031/shuhari/internal/receipt"
	contracts "github.com/shunk031/shuhari/schemas"
)

//go:embed prompts/application.md
var applicationPrompt string

//go:embed prompts/application-output.schema.json
var applicationOutputSchema []byte

type applicationJudgeInput struct {
	Prompt     string `json:"prompt"`
	SkillName  string `json:"skill_name"`
	Skill      string `json:"skill"`
	Transcript string `json:"transcript_jsonl"`
}

type applicationJudgeOutput struct {
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

type applicationArtifact struct {
	SchemaVersion string `json:"schema_version"`
	TargetRead    bool   `json:"target_read"`
	Verdict       string `json:"verdict"`
	Applied       bool   `json:"applied"`
	Evidence      string `json:"evidence"`
}

func classifyApplication(ctx context.Context, suite Suite, agent harness.Harness, config Config, security harness.SecurityResolution, runDir string, item Case, result harness.Result) (applicationArtifact, error) {
	if !result.TargetRead {
		artifact := applicationArtifact{
			SchemaVersion: applicationArtifactSchemaVersion,
			Verdict:       "not_consulted",
			Evidence:      "The target skill was not read.",
		}
		return artifact, writeApplicationArtifact(runDir, artifact)
	}

	skillContents, err := os.ReadFile(filepath.Join(suite.SkillPath, "SKILL.md"))
	if err != nil {
		return applicationArtifact{}, fmt.Errorf("read skill for application judge: %w", err)
	}
	input := applicationJudgeInput{Prompt: item.Prompt, SkillName: suite.SkillName, Skill: string(skillContents), Transcript: string(result.Transcript)}
	encoded, err := json.Marshal(input)
	if err != nil {
		return applicationArtifact{}, fmt.Errorf("encode application judge input: %w", err)
	}
	prompt := strings.TrimSpace(applicationPrompt) + "\n\n" + string(encoded)
	workDir, err := os.MkdirTemp("", "shuhari-trigger-judge-")
	if err != nil {
		return applicationArtifact{}, fmt.Errorf("create trigger judge work directory: %w", err)
	}
	defer os.RemoveAll(workDir)
	judged, err := agent.Run(ctx, harness.Request{WorkDir: workDir, Prompt: prompt, Model: config.Model, ReasoningEffort: config.ReasoningEffort, Security: security, Timeout: config.Timeout, OutputSchema: applicationOutputSchema})
	if err != nil {
		judgeErr := fmt.Errorf("run application judge; prompt is %d bytes: %w", len(prompt), err)
		if attempts := harness.AttemptsFromError(err); attempts.AttemptCount > 0 {
			if writeErr := receipt.WriteTiming(filepath.Join(runDir, "application-timing.json"), harness.Usage{}, 0, attempts); writeErr != nil {
				return applicationArtifact{}, errors.Join(judgeErr, writeErr)
			}
		}
		return applicationArtifact{}, judgeErr
	}
	if err := receipt.WriteTiming(filepath.Join(runDir, "application-timing.json"), judged.Usage, judged.Duration, judged.Attempts); err != nil {
		return applicationArtifact{}, err
	}
	output, err := decodeApplicationJudgeOutput(judged.Response)
	if err != nil {
		return applicationArtifact{}, err
	}
	artifact := applicationArtifact{
		SchemaVersion: applicationArtifactSchemaVersion,
		TargetRead:    true,
		Verdict:       output.Verdict,
		Applied:       output.Verdict == "applied",
		Evidence:      output.Evidence,
	}
	if err := writeApplicationArtifact(runDir, artifact); err != nil {
		return applicationArtifact{}, err
	}
	if output.Verdict == "ambiguous" {
		return artifact, fmt.Errorf("application verdict is ambiguous: %s", output.Evidence)
	}
	return artifact, nil
}

func decodeApplicationJudgeOutput(response string) (applicationJudgeOutput, error) {
	var output applicationJudgeOutput
	decoder := json.NewDecoder(bytes.NewBufferString(response))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return applicationJudgeOutput{}, fmt.Errorf("decode application judge response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return applicationJudgeOutput{}, errors.New("decode application judge response: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return applicationJudgeOutput{}, fmt.Errorf("decode application judge response: %w", err)
	}
	if output.Verdict != "applied" && output.Verdict != "declined" && output.Verdict != "ambiguous" {
		return applicationJudgeOutput{}, fmt.Errorf("application judge returned invalid verdict %q", output.Verdict)
	}
	if strings.TrimSpace(output.Evidence) == "" {
		return applicationJudgeOutput{}, errors.New("application judge returned blank evidence")
	}
	return output, nil
}

func writeApplicationArtifact(runDir string, artifact applicationArtifact) error {
	if err := contracts.Validate("trigger-application", artifact); err != nil {
		return err
	}
	return writeJSON(filepath.Join(runDir, "application.json"), artifact)
}
