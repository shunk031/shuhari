package harness

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestParseSandboxLevelRejectsLegacyCodexValuesWithReplacement(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		legacy      string
		replacement string
	}{
		{legacy: "workspace-write", replacement: "isolated"},
		{legacy: "danger-full-access", replacement: "unsandboxed"},
	} {
		t.Run(test.legacy, func(t *testing.T) {
			_, err := ParseSandboxLevel(test.legacy)
			if err == nil {
				t.Fatalf("ParseSandboxLevel(%q) unexpectedly succeeded", test.legacy)
			}
			if !strings.Contains(err.Error(), test.replacement) {
				t.Fatalf("error %q does not name replacement %q", err, test.replacement)
			}
		})
	}
}

func TestCodexResolveSecurityRequiresExplicitUnsandboxedNetworkAndAcknowledgement(t *testing.T) {
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "")
	agent := newCodex(Config{})
	policy := SecurityPolicy{Level: SandboxUnsandboxed, Network: true}

	if _, err := agent.ResolveSecurity(context.Background(), policy); err == nil {
		t.Fatal("ResolveSecurity() accepted unsandboxed without acknowledgement")
	}
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "1")
	policy.Network = false
	_, err := agent.ResolveSecurity(context.Background(), policy)
	if err == nil {
		t.Fatal("ResolveSecurity() accepted unsandboxed with network=false")
	}
	if !errors.Is(err, ErrUnsupportedSecurityPolicy) {
		t.Fatalf("error = %v, want ErrUnsupportedSecurityPolicy", err)
	}
}

func TestValidateSecurityResolutionRejectsWrongAdapterOutput(t *testing.T) {
	t.Parallel()

	policy := SecurityPolicy{Level: SandboxIsolated, Network: false}
	resolution := SecurityResolution{
		SandboxLevel:       SandboxReadOnly,
		NetworkAccess:      NetworkDenied,
		CredentialBoundary: CredentialBoundaryNone,
		Adapter: AdapterSecurity{
			Name:         "fake",
			NativeMode:   "wrong",
			PolicyDigest: "sha256:wrong",
		},
	}
	if err := ValidateSecurityResolution(policy, resolution); err == nil {
		t.Fatal("ValidateSecurityResolution() accepted a mutated resolution")
	}
}

func TestCodexResolveSecurityMapsNeutralLevelsWithoutDegradation(t *testing.T) {
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "1")
	agent := newCodex(Config{})

	for _, test := range []struct {
		policy   SecurityPolicy
		native   string
		boundary CredentialBoundary
	}{
		{policy: SecurityPolicy{Level: SandboxIsolated}, native: codexIsolatedNativeMode, boundary: CredentialBoundaryEnforced},
		{policy: SecurityPolicy{Level: SandboxReadOnly}, native: codexReadOnlyNativeMode, boundary: CredentialBoundaryEnforced},
		{policy: SecurityPolicy{Level: SandboxUnsandboxed, Network: true}, native: codexUnsandboxedNativeMode, boundary: CredentialBoundaryNone},
	} {
		resolution, err := agent.ResolveSecurity(context.Background(), test.policy)
		if err != nil {
			t.Fatalf("ResolveSecurity(%#v) error = %v", test.policy, err)
		}
		if resolution.SandboxLevel != test.policy.Level || resolution.Adapter.NativeMode != test.native || resolution.CredentialBoundary != test.boundary {
			t.Fatalf("ResolveSecurity(%#v) = %#v", test.policy, resolution)
		}
	}
}

func TestCodexRejectsMutatedResolvedNativePolicy(t *testing.T) {
	t.Parallel()

	agent := newCodex(Config{})
	resolution, err := agent.ResolveSecurity(context.Background(), SecurityPolicy{Level: SandboxIsolated})
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SecurityResolution){
		func(value *SecurityResolution) { value.Adapter.NativeMode = codexReadOnlyNativeMode },
		func(value *SecurityResolution) { value.Adapter.PolicyDigest = "sha256:" + strings.Repeat("0", 64) },
	} {
		changed := resolution
		mutate(&changed)
		if err := validateCodexSecurityResolution(changed); err == nil {
			t.Fatalf("validateCodexSecurityResolution() accepted mutation %#v", changed)
		}
	}
}
