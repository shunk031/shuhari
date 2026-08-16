//go:build linux && codex_sandbox

package harness

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	requireCodexSandboxTestEnv = "SHUHARI_REQUIRE_CODEX_SANDBOX_TEST"
	mutateCredentialDeniesEnv  = "SHUHARI_TEST_REMOVE_CREDENTIAL_DENIES"
	mutateFilesystemEnv        = "SHUHARI_TEST_WEAKEN_FILESYSTEM"
	mutateNetworkEnv           = "SHUHARI_TEST_FORCE_NETWORK_ENABLED"
)

func TestCodexProtectedSandboxConformance(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		codexSandboxUnavailable(t, "Codex CLI is not installed")
	}

	for _, level := range []SandboxLevel{SandboxIsolated, SandboxReadOnly} {
		for _, network := range []bool{false, true} {
			name := fmt.Sprintf("%s/network-%t", level, network)
			t.Run(name, func(t *testing.T) {
				root, err := os.MkdirTemp("/tmp", "shuhari-sandbox-test-")
				if err != nil {
					t.Fatal(err)
				}
				defer os.RemoveAll(root)

				workDir := filepath.Join(root, "work")
				outsideDir := filepath.Join(root, "outside")
				sourceHome := filepath.Join(workDir, "source-codex")
				codexHome := filepath.Join(workDir, "shuhari-codex-regression", "codex-home")
				for _, directory := range []string{workDir, outsideDir, sourceHome, codexHome} {
					if err := os.MkdirAll(directory, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				workspaceInput := filepath.Join(workDir, "input.txt")
				workspaceOutput := filepath.Join(workDir, "output.txt")
				outsideInput := filepath.Join(outsideDir, "input.txt")
				outsideOutput := filepath.Join(outsideDir, "output.txt")
				for _, path := range []string{workspaceInput, outsideInput} {
					if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(outsideDir, filepath.Join(workDir, "escape")); err != nil {
					t.Fatal(err)
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
				agent := newCodex(Config{})
				security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: level, Network: network})
				request := Request{WorkDir: workDir, Security: security}
				if err := writeCodexProfile(codexHome, request); err != nil {
					t.Fatal(err)
				}
				if os.Getenv(mutateCredentialDeniesEnv) == "1" {
					removeCredentialDenies(t, codexHome, sourceHome)
				}
				if os.Getenv(mutateFilesystemEnv) == "1" {
					weakenFilesystemPolicy(t, codexHome, outsideDir, level)
				}
				if os.Getenv(mutateNetworkEnv) == "1" {
					forceNetworkEnabled(t, codexHome)
				}

				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				defer server.Close()

				// `codex sandbox` resolves the same profile and starts the same child
				// sandbox used for model-generated commands, without requiring a model turn.
				attack := `
probe() {
  name="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf '%s=allowed\n' "$name"
  else
    printf '%s=denied\n' "$name"
  fi
}
probe workspace-read cat "$1"
probe workspace-write /bin/sh -c 'printf changed > "$1"' sh "$2"
probe outside-read cat "$3"
probe outside-write /bin/sh -c 'printf changed > "$1"' sh "$4"
probe symlink-read cat "$5/input.txt"
probe symlink-write /bin/sh -c 'printf changed > "$1/output.txt"' sh "$5"
probe network curl --noproxy '*' --silent --show-error --connect-timeout 2 --max-time 3 "$6"
printf 'source-attempted\n'
cat "$7"
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
					"/bin/sh", "-c", attack, "shuhari-conformance-probe",
					workspaceInput, workspaceOutput, outsideInput, outsideOutput,
					filepath.Join(workDir, "escape"), server.URL, sourceAuth,
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
				wantWorkspaceWrite := "denied"
				if level == SandboxIsolated {
					wantWorkspaceWrite = "allowed"
				}
				wantNetwork := "denied"
				if network {
					wantNetwork = "allowed"
				}
				for probe, want := range map[string]string{
					"workspace-read":  "allowed",
					"workspace-write": wantWorkspaceWrite,
					"outside-read":    "denied",
					"outside-write":   "denied",
					"symlink-read":    "denied",
					"symlink-write":   "denied",
					"network":         wantNetwork,
				} {
					if marker := probe + "=" + want; !strings.Contains(text, marker) {
						t.Errorf("%s policy: missing %q in child output:\n%s", level, marker, text)
					}
				}
				for name, secret := range map[string]string{
					"source auth":    sourceSecret,
					"temporary auth": temporarySecret,
				} {
					if strings.Contains(text, secret) {
						t.Errorf("child command read %s despite the %s profile", name, level)
					}
				}
			})
		}
	}
}

func weakenFilesystemPolicy(t *testing.T, codexHome, outsideDir string, level SandboxLevel) {
	t.Helper()
	path := filepath.Join(codexHome, "shuhari.config.toml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := string(contents)
	anchor := `":minimal" = "read"` + "\n"
	if !strings.Contains(profile, anchor) {
		t.Fatalf("cannot apply filesystem mutation; profile lacks %q", anchor)
	}
	profile = strings.Replace(profile, anchor, anchor+fmt.Sprintf("%s = \"write\"\n", tomlString(outsideDir)), 1)
	if level == SandboxReadOnly {
		readOnly := `"." = "read"`
		if !strings.Contains(profile, readOnly) {
			t.Fatalf("cannot apply read-only mutation; profile lacks %q", readOnly)
		}
		profile = strings.Replace(profile, readOnly, `"." = "write"`, 1)
	}
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
}

func forceNetworkEnabled(t *testing.T, codexHome string) {
	t.Helper()
	path := filepath.Join(codexHome, "shuhari.config.toml")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	profile := string(contents)
	if !strings.Contains(profile, "enabled = false") {
		return
	}
	profile = strings.Replace(profile, "enabled = false", "enabled = true", 1)
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexCommandEnvironmentStripsGitHubCredentials(t *testing.T) {
	codex, err := exec.LookPath("codex")
	if err != nil {
		codexSandboxUnavailable(t, "Codex CLI is not installed")
	}

	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "1")
	for _, level := range []SandboxLevel{SandboxIsolated, SandboxReadOnly, SandboxUnsandboxed} {
		t.Run(string(level), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "shuhari-env-test-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)

			workDir := filepath.Join(root, "work")
			codexHome := filepath.Join(root, "codex-home")
			if err := os.MkdirAll(codexHome, 0o700); err != nil {
				t.Fatal(err)
			}
			agent := newCodex(Config{})
			security := mustResolveCodexSecurity(t, agent, SecurityPolicy{Level: level, Network: true})
			if err := writeCodexProfile(codexHome, Request{WorkDir: workDir, Security: security}); err != nil {
				t.Fatal(err)
			}

			// Exercise the explicit credential filter independently from the
			// primary inherit-none policy, so a future inheritance change stays safe.
			profilePath := filepath.Join(codexHome, "shuhari.config.toml")
			profile, err := os.ReadFile(profilePath)
			if err != nil {
				t.Fatal(err)
			}
			profile = []byte(strings.Replace(string(profile), `inherit = "none"`, `inherit = "all"`, 1))
			if err := os.WriteFile(profilePath, profile, 0o600); err != nil {
				t.Fatal(err)
			}

			arguments := []string{"sandbox", "--profile", "shuhari", "--cd", workDir}
			if level == SandboxUnsandboxed {
				arguments = append(arguments, "--permission-profile", ":danger-full-access")
			} else {
				arguments = append(arguments, "--permission-profile", "shuhari-eval")
			}
			arguments = append(arguments, "/bin/sh", "-c", `env | grep -E '^(GH|GITHUB)_' || true`)
			command := exec.Command(codex, arguments...)
			command.Env = append(cleanEnvironment(codexHome),
				"GH_TOKEN=synthetic-gh-token",
				"GITHUB_TOKEN=synthetic-github-token",
				"GH_ENTERPRISE_TOKEN=synthetic-gh-enterprise-token",
				"GITHUB_ENTERPRISE_TOKEN=synthetic-github-enterprise-token",
				"GH_AUTH=synthetic-gh-auth",
				"GITHUB_CREDENTIAL=synthetic-github-credential",
			)
			output, err := command.Output()
			if err != nil {
				failureOutput := output
				if exitError, ok := err.(*exec.ExitError); ok {
					failureOutput = append(failureOutput, exitError.Stderr...)
				}
				if codexSandboxCapabilityError(failureOutput) {
					codexSandboxUnavailable(t, strings.TrimSpace(string(failureOutput)))
				}
				t.Fatalf("run Codex command environment probe: %v\n%s", err, failureOutput)
			}
			if strings.TrimSpace(string(output)) != "" {
				t.Fatalf("evaluated command received GitHub credential variables in %s mode:\n%s", level, output)
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
		"bubblewrap is unavailable",
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
