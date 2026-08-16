package eval

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func createIteration(suite Suite, configured string) (string, error) {
	root := configured
	if root == "" {
		root = filepath.Join(filepath.Dir(suite.Root), safeName(suite.Name)+"-workspace")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	next := 1
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return "", fmt.Errorf("read workspace: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "iteration-") {
			continue
		}
		value, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "iteration-"))
		if err == nil && value >= next {
			next = value + 1
		}
	}
	iteration := filepath.Join(absolute, fmt.Sprintf("iteration-%d", next))
	if err := os.Mkdir(iteration, 0o755); err != nil {
		return "", fmt.Errorf("create iteration: %w", err)
	}
	return iteration, nil
}

func runDirectory(iteration, caseID, variant string, trial int) string {
	base := filepath.Join(iteration, "eval-"+safeName(caseID), variant)
	if trial == 1 {
		return base
	}
	return filepath.Join(base, "trials", strconv.Itoa(trial))
}

func safeName(value string) string {
	clean := strings.Trim(unsafePath.ReplaceAllString(value, "-"), "-.")
	if clean == "" {
		return "case"
	}
	return clean
}

func stageFixtures(suite Suite, item Case, workDir string) ([]string, error) {
	staged := make([]string, 0, len(item.Files))
	for _, relative := range item.Files {
		source := filepath.Join(suite.Root, filepath.Clean(relative))
		destination := filepath.Join(workDir, filepath.Clean(relative))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return nil, fmt.Errorf("create fixture directory: %w", err)
		}
		contents, err := os.ReadFile(source)
		if err != nil {
			return nil, fmt.Errorf("read fixture %q: %w", relative, err)
		}
		if err := os.WriteFile(destination, contents, 0o644); err != nil {
			return nil, fmt.Errorf("stage fixture %q: %w", relative, err)
		}
		staged = append(staged, filepath.ToSlash(relative))
	}
	return staged, nil
}

func copyOutputs(source, destination string) error {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	info, err := os.Stat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect agent outputs: %w", err)
	}
	if !info.IsDir() {
		return errors.New("agent outputs path is not a directory")
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.Type()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read agent output symlink %s: %w", relative, err)
			}
			if symlinkStaysWithin(source, path, linkTarget) {
				if err := os.Symlink(linkTarget, target); err != nil {
					return fmt.Errorf("preserve agent output symlink %s: %w", relative, err)
				}
				return nil
			}
			return writeSymlinkMetadata(target, filepath.ToSlash(relative), linkTarget)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, contents, info.Mode().Perm())
	})
}

func symlinkStaysWithin(root, linkPath, linkTarget string) bool {
	if filepath.IsAbs(linkTarget) {
		return false
	}
	candidate := filepath.Clean(filepath.Join(filepath.Dir(linkPath), linkTarget))
	if !pathWithin(root, candidate) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	return err == nil && pathWithin(root, resolved)
}

func pathWithin(root, candidate string) bool {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	absoluteCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteCandidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func writeSymlinkMetadata(path, link, target string) error {
	metadata := struct {
		Type   string `json:"type"`
		Link   string `json:"link"`
		Target string `json:"target"`
		Note   string `json:"note"`
	}{
		Type:   "symlink",
		Link:   link,
		Target: target,
		Note:   "The symlink was recorded as metadata and not followed because its target was absolute, escaped, or could not be resolved within the collected output tree.",
	}
	contents, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("encode agent output symlink metadata %s: %w", link, err)
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return fmt.Errorf("write agent output symlink metadata %s: %w", link, err)
	}
	return nil
}

func writeJSON(path string, value any) error {
	contents, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	contents = append(contents, '\n')
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func renderArtifact(outputDir string) (string, error) {
	var paths []string
	if err := filepath.WalkDir(outputDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		return "", fmt.Errorf("walk output artifacts: %w", err)
	}
	sort.Strings(paths)
	var builder strings.Builder
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("inspect output artifact: %w", err)
		}
		relative, _ := filepath.Rel(outputDir, path)
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return "", fmt.Errorf("read output artifact symlink: %w", err)
			}
			fmt.Fprintf(&builder, "\n--- symlink: %s -> %s ---\n", filepath.ToSlash(relative), target)
			builder.WriteString("preserved because the target resolves within the collected output tree\n")
			continue
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read output artifact: %w", err)
		}
		fmt.Fprintf(&builder, "\n--- file: %s (%d bytes) ---\n", filepath.ToSlash(relative), len(contents))
		if len(contents) <= 64*1024 && utf8.Valid(contents) && !bytes.ContainsRune(contents, '\x00') {
			builder.Write(contents)
			if len(contents) == 0 || contents[len(contents)-1] != '\n' {
				builder.WriteByte('\n')
			}
		} else {
			digest := sha256.Sum256(contents)
			fmt.Fprintf(&builder, "binary sha256=%s\n", hex.EncodeToString(digest[:]))
		}
	}
	return strings.TrimSpace(builder.String()), nil
}

func suiteDigest(suite Suite) (string, error) {
	hash := sha256.New()
	paths := []string{suite.TargetPath, suite.EvalPath}
	for _, item := range suite.Cases {
		for _, file := range item.Files {
			paths = append(paths, filepath.Join(suite.Root, filepath.Clean(file)))
		}
	}
	if suite.Kind == "skill" {
		paths = nil
		if err := filepath.WalkDir(suite.Root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() {
				paths = append(paths, path)
			}
			return nil
		}); err != nil {
			return "", fmt.Errorf("walk target: %w", err)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("target contains a symlink: %s", path)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("hash %s: %w", path, err)
		}
		relative, _ := filepath.Rel(suite.Root, path)
		_, _ = hash.Write([]byte(filepath.ToSlash(relative)))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(contents)
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
