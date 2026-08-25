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

	_, err := agent.ResolveSecurity(context.Background(), policy)
	if err == nil {
		t.Fatal("ResolveSecurity() accepted unsandboxed without acknowledgement")
	}
	if !errors.Is(err, ErrUnsupportedSecurityPolicy) {
		t.Fatalf("missing acknowledgement error = %v, want ErrUnsupportedSecurityPolicy", err)
	}
	t.Setenv(NoCredentialBoundaryAcknowledgementEnv, "1")
	policy.Network = false
	_, err = agent.ResolveSecurity(context.Background(), policy)
	if err == nil {
		t.Fatal("ResolveSecurity() accepted unsandboxed with network=false")
	}
	if !errors.Is(err, ErrUnsupportedSecurityPolicy) {
		t.Fatalf("error = %v, want ErrUnsupportedSecurityPolicy", err)
	}
}

func TestValidateSecurityResolutionRejectsMalformedPolicyDigest(t *testing.T) {
	t.Parallel()

	policy := SecurityPolicy{Level: SandboxIsolated, Network: false}
	resolution := SecurityResolution{
		SandboxLevel:       SandboxIsolated,
		NetworkAccess:      NetworkDenied,
		CredentialBoundary: CredentialBoundaryEnforced,
		Adapter: AdapterSecurity{
			Name:         "fake",
			NativeMode:   "fake-isolated",
			PolicyDigest: "x",
		},
	}
	if err := ValidateSecurityResolution(policy, resolution); err == nil {
		t.Fatal("ValidateSecurityResolution() accepted a digest outside the artifact schema")
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

func TestSecurityPolicyEqual(t *testing.T) {
	base := SecurityPolicy{Level: SandboxIsolated, Network: true, HostTools: []string{"uv", "prek"}}
	for name, other := range map[string]SecurityPolicy{
		"different level":   {Level: SandboxReadOnly, Network: true, HostTools: []string{"uv", "prek"}},
		"different network": {Level: SandboxIsolated, Network: false, HostTools: []string{"uv", "prek"}},
		"fewer tools":       {Level: SandboxIsolated, Network: true, HostTools: []string{"uv"}},
		"different tool":    {Level: SandboxIsolated, Network: true, HostTools: []string{"uv", "just"}},
		"reordered tools":   {Level: SandboxIsolated, Network: true, HostTools: []string{"prek", "uv"}},
		"no tools at all":   {Level: SandboxIsolated, Network: true},
	} {
		if base.Equal(other) {
			t.Fatalf("Equal() reported %s as the same boundary", name)
		}
	}

	same := SecurityPolicy{Level: SandboxIsolated, Network: true, HostTools: []string{"uv", "prek"}}
	if !base.Equal(same) {
		t.Fatal("Equal() reported two identical policies as different")
	}
}

func TestSecurityResolutionEqual(t *testing.T) {
	adapter := AdapterSecurity{Name: "codex", NativeMode: "workspace-write", PolicyDigest: "sha256:abc"}
	base := SecurityResolution{
		SandboxLevel:       SandboxIsolated,
		NetworkAccess:      NetworkAllowed,
		CredentialBoundary: CredentialBoundaryEnforced,
		Adapter:            adapter,
		HostTools:          []string{"uv"},
	}
	for name, mutate := range map[string]func(SecurityResolution) SecurityResolution{
		"different level":    func(r SecurityResolution) SecurityResolution { r.SandboxLevel = SandboxReadOnly; return r },
		"different network":  func(r SecurityResolution) SecurityResolution { r.NetworkAccess = NetworkDenied; return r },
		"different boundary": func(r SecurityResolution) SecurityResolution { r.CredentialBoundary = CredentialBoundaryNone; return r },
		"different digest": func(r SecurityResolution) SecurityResolution {
			r.Adapter.PolicyDigest = "sha256:def"
			return r
		},
		"different tools": func(r SecurityResolution) SecurityResolution { r.HostTools = []string{"prek"}; return r },
		"no tools":        func(r SecurityResolution) SecurityResolution { r.HostTools = nil; return r },
	} {
		if base.Equal(mutate(base)) {
			t.Fatalf("Equal() reported %s as the same boundary", name)
		}
	}

	if !base.Equal(base) {
		t.Fatal("Equal() reported a resolution as different from itself")
	}
}
