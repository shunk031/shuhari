package eval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shunk031/shuhari/internal/harness"
	"github.com/shunk031/shuhari/internal/skill"
)

type caseID string

func (id *caseID) UnmarshalJSON(contents []byte) error {
	var text string
	if err := json.Unmarshal(contents, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return errors.New("case id must not be empty")
		}
		*id = caseID(text)
		return nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return errors.New("case id must be a string or number")
	}
	if _, err := strconv.ParseInt(number.String(), 10, 64); err != nil {
		return errors.New("numeric case id must be an integer")
	}
	*id = caseID(number.String())
	return nil
}

type rawCase struct {
	ID             caseID   `json:"id"`
	Prompt         string   `json:"prompt"`
	ExpectedOutput string   `json:"expected_output"`
	Files          []string `json:"files"`
	Assertions     []string `json:"assertions"`
}

func LoadSkillSuite(skillPath string) (Suite, error) {
	absolute, err := filepath.Abs(skillPath)
	if err != nil {
		return Suite{}, fmt.Errorf("resolve skill path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Suite{}, fmt.Errorf("inspect skill path: %w", err)
	}
	if !info.IsDir() {
		return Suite{}, fmt.Errorf("skill path is not a directory: %s", absolute)
	}
	metadata, err := skill.Load(absolute)
	if err != nil {
		return Suite{}, err
	}
	evalPath := filepath.Join(absolute, "evals", "evals.json")
	var raw struct {
		SkillName string    `json:"skill_name"`
		Evals     []rawCase `json:"evals"`
	}
	if err := decodeStrictJSON(evalPath, &raw); err != nil {
		return Suite{}, err
	}
	if raw.SkillName != metadata.Name {
		return Suite{}, fmt.Errorf("eval skill_name %q does not match SKILL.md name %q", raw.SkillName, metadata.Name)
	}
	cases, err := validateCases(absolute, raw.Evals)
	if err != nil {
		return Suite{}, err
	}
	return Suite{Kind: harness.TargetSkill, Name: metadata.Name, Root: absolute, TargetPath: absolute, EvalPath: evalPath, Cases: cases}, nil
}

func LoadInstructionsSuite(instructionsPath, evalPath string) (Suite, error) {
	absolute, err := filepath.Abs(instructionsPath)
	if err != nil {
		return Suite{}, fmt.Errorf("resolve instructions path: %w", err)
	}
	if info, err := os.Stat(absolute); err != nil || info.IsDir() {
		if err != nil {
			return Suite{}, fmt.Errorf("inspect instructions path: %w", err)
		}
		return Suite{}, fmt.Errorf("instructions path is not a file: %s", absolute)
	}
	if evalPath == "" {
		extension := filepath.Ext(absolute)
		evalPath = strings.TrimSuffix(absolute, extension) + ".evals.json"
	} else {
		evalPath, err = filepath.Abs(evalPath)
		if err != nil {
			return Suite{}, fmt.Errorf("resolve eval path: %w", err)
		}
	}
	var raw struct {
		InstructionsName string    `json:"instructions_name"`
		Evals            []rawCase `json:"evals"`
	}
	if err := decodeStrictJSON(evalPath, &raw); err != nil {
		return Suite{}, err
	}
	if strings.TrimSpace(raw.InstructionsName) == "" {
		return Suite{}, errors.New("instructions_name must not be empty")
	}
	root := filepath.Dir(absolute)
	cases, err := validateCases(root, raw.Evals)
	if err != nil {
		return Suite{}, err
	}
	return Suite{Kind: harness.TargetInstructions, Name: raw.InstructionsName, Root: root, TargetPath: absolute, EvalPath: evalPath, Cases: cases}, nil
}

func decodeStrictJSON(path string, destination any) error {
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("decode %s: trailing JSON value", path)
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

func validateCases(root string, raw []rawCase) ([]Case, error) {
	if len(raw) == 0 {
		return nil, errors.New("evals must contain at least one case")
	}
	seen := map[string]bool{}
	seenPaths := map[string]string{}
	cases := make([]Case, 0, len(raw))
	for index, item := range raw {
		identifier := string(item.ID)
		if identifier == "" {
			return nil, fmt.Errorf("evals[%d].id must not be empty", index)
		}
		if seen[identifier] {
			return nil, fmt.Errorf("duplicate eval id %q", identifier)
		}
		seen[identifier] = true
		pathName := safeName(identifier)
		if previous, exists := seenPaths[pathName]; exists {
			return nil, fmt.Errorf("eval ids %q and %q map to the same workspace path %q", previous, identifier, pathName)
		}
		seenPaths[pathName] = identifier
		if strings.TrimSpace(item.Prompt) == "" || strings.TrimSpace(item.ExpectedOutput) == "" {
			return nil, fmt.Errorf("evals[%d] requires prompt and expected_output", index)
		}
		for _, file := range item.Files {
			if err := validateFixture(root, file); err != nil {
				return nil, fmt.Errorf("evals[%d].files: %w", index, err)
			}
		}
		assertions := make([]string, 0, len(item.Assertions))
		for _, assertion := range item.Assertions {
			text := strings.TrimSpace(assertion)
			if text == "" {
				return nil, fmt.Errorf("evals[%d].assertions contains an empty assertion", index)
			}
			assertions = append(assertions, text)
		}
		cases = append(cases, Case{ID: identifier, Prompt: item.Prompt, ExpectedOutput: item.ExpectedOutput, Files: item.Files, Assertions: assertions})
	}
	return cases, nil
}

func validateFixture(root, relative string) error {
	if filepath.IsAbs(relative) || relative == "" {
		return fmt.Errorf("fixture path must be relative: %q", relative)
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("fixture escapes target root: %q", relative)
	}
	absolute := filepath.Join(root, clean)
	info, err := os.Stat(absolute)
	if err != nil {
		return fmt.Errorf("inspect fixture %q: %w", relative, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("fixture is not a regular file: %q", relative)
	}
	return nil
}
