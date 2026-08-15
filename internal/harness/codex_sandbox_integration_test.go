//go:build linux && codex_sandbox

package harness

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	requireCodexSandboxTestEnv = "SHUHARI_REQUIRE_CODEX_SANDBOX_TEST"
	mutateCredentialDeniesEnv  = "SHUHARI_TEST_REMOVE_CREDENTIAL_DENIES"
)

func TestCodexDefaultSandboxDeniesChildCredentialReads(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		codexSandboxUnavailable(t, "Codex CLI is not installed")
	}

	for _, sandbox := range []string{"workspace-write", "read-only"} {
		t.Run(sandbox, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "shuhari-sandbox-test-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)

			workDir := filepath.Join(root, "work")
			sourceHome := filepath.Join(workDir, "source-codex")
			codexHome := filepath.Join(workDir, "shuhari-codex-regression", "codex-home")
			for _, directory := range []string{sourceHome, codexHome} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			sourceSecret := "synthetic-source-auth-material"
			temporarySecret := "synthetic-temporary-auth-material"
			sourceAuth := filepath.Join(sourceHome, "auth.json")
			temporaryAuth := filepath.Join(codexHome, "auth.json")
			if err := os.WriteFile(sourceAuth, []byte(sourceSecret), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(temporaryAuth, []byte(temporarySecret), 0o600); err != nil {
				t.Fatal(err)
			}

			t.Setenv("CODEX_HOME", sourceHome)
			request := Request{WorkDir: workDir, Sandbox: sandbox}
			if err := writeCodexProfile(codexHome, request); err != nil {
				t.Fatal(err)
			}
			if os.Getenv(mutateCredentialDeniesEnv) == "1" {
				removeCredentialDenies(t, codexHome, sourceHome)
			}

			// `codex sandbox` resolves the same profile and starts the same child
			// sandbox used for model-generated commands, without requiring a model turn.
			attack := `
printf 'source-attempted\n'
cat "$1"
printf '\ntemporary-attempted\n'
find /tmp -path '*/shuhari-codex-*/codex-home/auth.json' -exec cat {} \;
printf '\nprobe-completed\n'
`
			command := exec.Command(
				codex,
				"sandbox",
				"--profile", "shuhari",
				"--permission-profile", "shuhari-eval",
				"--cd", workDir,
				"/bin/sh", "-c", attack, "shuhari-credential-probe", sourceAuth,
			)
			command.Env = cleanEnvironment(codexHome)
			output, err := command.CombinedOutput()
			if err != nil {
				if codexSandboxCapabilityError(output) {
					codexSandboxUnavailable(t, strings.TrimSpace(string(output)))
				}
				t.Fatalf("run Codex child sandbox: %v\n%s", err, output)
			}

			text := string(output)
			for _, marker := range []string{"source-attempted", "temporary-attempted", "probe-completed"} {
				if !strings.Contains(text, marker) {
					t.Fatalf("credential probe did not execute %q:\n%s", marker, text)
				}
			}
			for name, secret := range map[string]string{
				"source auth":    sourceSecret,
				"temporary auth": temporarySecret,
			} {
				if strings.Contains(text, secret) {
					t.Errorf("child command read %s despite the %s profile", name, sandbox)
				}
			}
		})
	}
}

func removeCredentialDenies(t *testing.T, codexHome, sourceHome string) {
	t.Helper()
	path := filepath.Join(codexHome, "shuhari.config.toml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := string(contents)
	for _, denied := range []string{codexHome, sourceHome} {
		entry := fmt.Sprintf("%s = \"deny\"\n", tomlString(denied))
		if !strings.Contains(profile, entry) {
			t.Fatalf("cannot apply credential-deny mutation; profile lacks %q", entry)
		}
		profile = strings.Replace(profile, entry, "", 1)
	}
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
}

func codexSandboxUnavailable(t *testing.T, reason string) {
	t.Helper()
	if os.Getenv(requireCodexSandboxTestEnv) == "1" {
		t.Fatalf("Codex sandbox regression test is required: %s", reason)
	}
	t.Skipf("Codex sandbox is unavailable: %s", reason)
}

func codexSandboxCapabilityError(output []byte) bool {
	message := strings.ToLower(string(output))
	for _, marker := range []string{
		"no permissions to create new namespace",
		"kernel does not allow non-privileged user namespaces",
		"failed to create user namespace",
		"operation not permitted",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
