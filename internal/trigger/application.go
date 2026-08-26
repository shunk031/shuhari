package trigger

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/shunk031/shuhari/internal/harness"
	"github.com/shunk031/shuhari/internal/receipt"
	"github.com/shunk031/shuhari/internal/skill"
	contracts "github.com/shunk031/shuhari/schemas"
)

//go:embed prompts/application.md
var applicationPrompt string

//go:embed prompts/application-output.schema.json
var applicationOutputSchema []byte

//go:embed prompts/completion.md
var completionPrompt string

//go:embed prompts/completion-output.schema.json
var completionOutputSchema []byte

const (
	maxApplicationJudgePromptBytes     = 512 * 1024
	maxApplicationJudgeSkillBytes      = 64 * 1024
	maxApplicationJudgeTranscriptBytes = 128 * 1024
	maxTranscriptCommandOutputBytes    = 1024
	maxTranscriptAgentMessageBytes     = 16 * 1024
)

type applicationJudgeInput struct {
	Prompt              string `json:"prompt"`
	SkillName           string `json:"skill_name"`
	Skill               string `json:"skill"`
	SkillBytes          int    `json:"skill_bytes"`
	SkillSHA256         string `json:"skill_sha256"`
	SkillTruncated      bool   `json:"skill_truncated"`
	Transcript          string `json:"transcript_jsonl"`
	TranscriptBytes     int    `json:"transcript_bytes"`
	TranscriptSHA256    string `json:"transcript_sha256"`
	TranscriptCompacted bool   `json:"transcript_compacted"`
	TranscriptTruncated bool   `json:"transcript_truncated"`
}

type applicationJudgeOutput struct {
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence"`
}

type applicationArtifact struct {
	SchemaVersion string       `json:"schema_version"`
	Mode          harness.Mode `json:"mode,omitempty"`
	TargetRead    *bool        `json:"target_read,omitempty"`
	Verdict       string       `json:"verdict"`
	Applied       *bool        `json:"applied,omitempty"`
	TargetInvoked *bool        `json:"target_invoked,omitempty"`
	Evidence      string       `json:"evidence"`
	Decision      *Decision    `json:"decision,omitempty"`
}

type completionDecisionInput struct {
	Prompt           string `json:"prompt"`
	SkillName        string `json:"skill_name"`
	SkillDescription string `json:"skill_description"`
}

type completionDecisionOutput struct {
	Invoke *bool  `json:"invoke"`
	Reason string `json:"reason"`
}

func executeCompletionCase(ctx context.Context, suite Suite, agent harness.Harness, config Config, runDir string, item Case, trial int) (applicationArtifact, error) {
	metadata, err := skill.Load(suite.SkillPath)
	if err != nil {
		return applicationArtifact{}, err
	}
	input, err := json.Marshal(completionDecisionInput{Prompt: item.Prompt, SkillName: suite.SkillName, SkillDescription: metadata.Description})
	if err != nil {
		return applicationArtifact{}, fmt.Errorf("encode completion trigger input: %w", err)
	}
	prompt := strings.TrimSpace(completionPrompt) + "\n\n" + string(input)
	if len(prompt) > maxApplicationJudgePromptBytes {
		return applicationArtifact{}, fmt.Errorf("input_too_large: completion trigger prompt is %d bytes; limit is %d bytes", len(prompt), maxApplicationJudgePromptBytes)
	}
	result, err := agent.Run(ctx, harness.Request{Mode: harness.ModeCompletion, Prompt: prompt, Model: config.Model, ReasoningEffort: config.ReasoningEffort, Timeout: config.Timeout, OutputSchema: completionOutputSchema})
	if err != nil {
		if attempts := harness.AttemptsFromError(err); attempts.AttemptCount > 0 {
			if writeErr := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), harness.Usage{}, 0, attempts); writeErr != nil {
				return applicationArtifact{}, errors.Join(err, writeErr)
			}
		}
		return applicationArtifact{}, fmt.Errorf("%s trial %d: run completion trigger: %w", item.ID, trial, err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "transcript.jsonl"), result.Transcript, 0o644); err != nil {
		return applicationArtifact{}, fmt.Errorf("write trigger transcript: %w", err)
	}
	if err := receipt.WriteTiming(filepath.Join(runDir, "timing.json"), result.Usage, result.Duration, result.Attempts); err != nil {
		return applicationArtifact{}, err
	}
	output, err := decodeCompletionDecision(result.Response)
	if err != nil {
		return applicationArtifact{}, err
	}
	decision := Decision{Invoke: *output.Invoke, Reason: output.Reason}
	verdict := "not_invoked"
	if *output.Invoke {
		verdict = "invoked"
	}
	artifact := applicationArtifact{SchemaVersion: applicationArtifactSchemaVersion, Mode: harness.ModeCompletion, Verdict: verdict, TargetInvoked: boolPointer(*output.Invoke), Evidence: output.Reason, Decision: &decision}
	if err := writeApplicationArtifact(runDir, artifact); err != nil {
		return applicationArtifact{}, err
	}
	return artifact, nil
}

func classifyApplication(ctx context.Context, suite Suite, agent harness.Harness, config Config, security harness.SecurityResolution, runDir string, item Case, result harness.Result) (applicationArtifact, error) {
	if !result.TargetRead {
		artifact := applicationArtifact{
			SchemaVersion: applicationArtifactSchemaVersion,
			Mode:          harness.ModeAgentic,
			TargetRead:    boolPointer(false),
			Verdict:       "not_consulted",
			Applied:       boolPointer(false),
			Evidence:      "The target skill was not read.",
		}
		return artifact, writeApplicationArtifact(runDir, artifact)
	}

	skillContents, err := os.ReadFile(filepath.Join(suite.SkillPath, "SKILL.md"))
	if err != nil {
		return applicationArtifact{}, fmt.Errorf("read skill for application judge: %w", err)
	}
	skillText, skillBytes, skillDigest, skillTruncated := boundedApplicationText(skillContents, maxApplicationJudgeSkillBytes)
	compactTranscript, transcriptTruncated := compactApplicationTranscript(result.Transcript, maxApplicationJudgeTranscriptBytes)
	input := applicationJudgeInput{
		Prompt: item.Prompt, SkillName: suite.SkillName,
		Skill: skillText, SkillBytes: skillBytes, SkillSHA256: skillDigest, SkillTruncated: skillTruncated,
		Transcript: compactTranscript, TranscriptBytes: len(result.Transcript), TranscriptSHA256: digestBytes(result.Transcript),
		TranscriptCompacted: true, TranscriptTruncated: transcriptTruncated,
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return applicationArtifact{}, fmt.Errorf("encode application judge input: %w", err)
	}
	prompt := strings.TrimSpace(applicationPrompt) + "\n\n" + string(encoded)
	if len(prompt) > maxApplicationJudgePromptBytes {
		return applicationArtifact{}, fmt.Errorf("input_too_large: application judge prompt is %d bytes; limit is %d bytes", len(prompt), maxApplicationJudgePromptBytes)
	}
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
		Mode:          harness.ModeAgentic,
		TargetRead:    boolPointer(true),
		Verdict:       output.Verdict,
		Applied:       boolPointer(output.Verdict == "applied"),
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

func boolPointer(value bool) *bool {
	return &value
}

func digestBytes(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func boundedApplicationText(contents []byte, limit int) (string, int, string, bool) {
	digest := digestBytes(contents)
	if len(contents) <= limit {
		return string(contents), len(contents), digest, false
	}
	marker := fmt.Sprintf("\n[shuhari input truncated: original_bytes=%d sha256=%s]\n", len(contents), digest)
	available := limit - len(marker)
	if available <= 0 {
		return marker, len(contents), digest, true
	}
	prefix := available / 2
	suffix := available - prefix
	return string(contents[:prefix]) + marker + string(contents[len(contents)-suffix:]), len(contents), digest, true
}

func compactApplicationTranscript(transcript []byte, limit int) (string, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(transcript))
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var compact strings.Builder
	for scanner.Scan() {
		line := scanner.Bytes()
		var event map[string]json.RawMessage
		if err := json.Unmarshal(line, &event); err != nil {
			bounded, _, _, _ := boundedApplicationText(line, 1024)
			compact.WriteString(bounded)
			compact.WriteByte('\n')
			continue
		}
		summary := map[string]any{"type": rawJSONText(event["type"])}
		if rawItem := event["item"]; len(rawItem) > 0 {
			var item map[string]json.RawMessage
			if json.Unmarshal(rawItem, &item) == nil {
				itemSummary := map[string]any{"type": rawJSONText(item["type"])}
				switch rawJSONText(item["type"]) {
				case "command_execution":
					for _, key := range []string{"command", "status", "exit_code"} {
						if value := item[key]; len(value) > 0 {
							itemSummary[key] = json.RawMessage(value)
						}
					}
					if value := item["aggregated_output"]; len(value) > 0 {
						var output string
						if json.Unmarshal(value, &output) == nil {
							bounded, _, _, _ := boundedApplicationText([]byte(output), maxTranscriptCommandOutputBytes)
							itemSummary["aggregated_output"] = bounded
						}
					}
				case "agent_message":
					var text string
					if json.Unmarshal(item["text"], &text) == nil {
						bounded, _, _, _ := boundedApplicationText([]byte(text), maxTranscriptAgentMessageBytes)
						itemSummary["text"] = bounded
					}
				}
				summary["item"] = itemSummary
			}
		} else if value := event["error"]; len(value) > 0 {
			summary["error"] = json.RawMessage(value)
		}
		encoded, err := json.Marshal(summary)
		if err == nil {
			compact.Write(encoded)
			compact.WriteByte('\n')
		}
	}
	if scanner.Err() != nil {
		bounded, _, _, _ := boundedApplicationText(transcript, limit)
		return bounded, true
	}
	bounded, _, _, truncated := boundedApplicationText([]byte(compact.String()), limit)
	return bounded, truncated
}

func rawJSONText(value json.RawMessage) string {
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	return "unknown"
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

func decodeCompletionDecision(response string) (completionDecisionOutput, error) {
	var output completionDecisionOutput
	decoder := json.NewDecoder(bytes.NewBufferString(response))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&output); err != nil {
		return completionDecisionOutput{}, fmt.Errorf("decode completion trigger response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return completionDecisionOutput{}, errors.New("decode completion trigger response: trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return completionDecisionOutput{}, fmt.Errorf("decode completion trigger response: %w", err)
	}
	if output.Invoke == nil {
		return completionDecisionOutput{}, errors.New("completion trigger response omitted invoke")
	}
	if strings.TrimSpace(output.Reason) == "" {
		return completionDecisionOutput{}, errors.New("completion trigger response has blank reason")
	}
	return output, nil
}

func writeApplicationArtifact(runDir string, artifact applicationArtifact) error {
	if err := contracts.Validate("trigger-application", artifact); err != nil {
		return err
	}
	return writeJSON(filepath.Join(runDir, "application.json"), artifact)
}
