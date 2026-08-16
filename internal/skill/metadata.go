package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Metadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func Load(directory string) (Metadata, error) {
	path := filepath.Join(directory, "SKILL.md")
	contents, err := os.ReadFile(path)
	if err != nil {
		return Metadata{}, fmt.Errorf("read SKILL.md: %w", err)
	}
	lines := strings.Split(string(contents), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return Metadata{}, errors.New("SKILL.md must start with YAML frontmatter")
	}
	end := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			end = index
			break
		}
	}
	if end < 0 {
		return Metadata{}, errors.New("SKILL.md frontmatter is not closed")
	}
	var metadata Metadata
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &metadata); err != nil {
		return Metadata{}, fmt.Errorf("decode SKILL.md frontmatter: %w", err)
	}
	if strings.TrimSpace(metadata.Name) == "" || strings.TrimSpace(metadata.Description) == "" {
		return Metadata{}, errors.New("SKILL.md frontmatter requires name and description")
	}
	if metadata.Name != filepath.Base(directory) {
		return Metadata{}, fmt.Errorf("SKILL.md name %q does not match directory %q", metadata.Name, filepath.Base(directory))
	}
	return metadata, nil
}
