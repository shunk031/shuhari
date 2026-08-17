package harness

import (
	"errors"
	"fmt"
	"os"
	"regexp"
)

type SandboxLevel string

const (
	SandboxIsolated    SandboxLevel = "isolated"
	SandboxReadOnly    SandboxLevel = "read-only"
	SandboxUnsandboxed SandboxLevel = "unsandboxed"
)

type NetworkAccess string

const (
	NetworkDenied  NetworkAccess = "denied"
	NetworkAllowed NetworkAccess = "allowed"
)

type CredentialBoundary string

const (
	CredentialBoundaryEnforced CredentialBoundary = "enforced"
	CredentialBoundaryNone     CredentialBoundary = "none"
)

const NoCredentialBoundaryAcknowledgementEnv = "SHUHARI_I_UNDERSTAND_NO_CREDENTIAL_BOUNDARY"

var ErrUnsupportedSecurityPolicy = errors.New("unsupported security policy")

var policyDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]+$`)

type UnsupportedSecurityPolicyError struct {
	Adapter string
	Policy  SecurityPolicy
	Reason  string
}

func (e *UnsupportedSecurityPolicyError) Error() string {
	adapter := e.Adapter
	if adapter == "" {
		adapter = "selected adapter"
	}
	return fmt.Sprintf("%s cannot honor sandbox level %q: %s", adapter, e.Policy.Level, e.Reason)
}

func (e *UnsupportedSecurityPolicyError) Unwrap() error {
	return ErrUnsupportedSecurityPolicy
}

type SecurityPolicy struct {
	Level   SandboxLevel `json:"sandbox_level"`
	Network bool         `json:"network"`
}

type AdapterSecurity struct {
	Name         string `json:"name"`
	NativeMode   string `json:"native_mode"`
	PolicyDigest string `json:"policy_digest"`
}

type SecurityResolution struct {
	SandboxLevel       SandboxLevel       `json:"sandbox_level"`
	NetworkAccess      NetworkAccess      `json:"network_access"`
	CredentialBoundary CredentialBoundary `json:"credential_boundary"`
	Adapter            AdapterSecurity    `json:"adapter"`
}

func (r SecurityResolution) Policy() SecurityPolicy {
	return SecurityPolicy{Level: r.SandboxLevel, Network: r.NetworkAccess == NetworkAllowed}
}

func EffectiveSandboxLevel(requested string) (SandboxLevel, error) {
	if requested == "" {
		requested = string(SandboxIsolated)
	}
	return ParseSandboxLevel(requested)
}

func ParseSandboxLevel(value string) (SandboxLevel, error) {
	switch SandboxLevel(value) {
	case SandboxIsolated, SandboxReadOnly, SandboxUnsandboxed:
		return SandboxLevel(value), nil
	case "workspace-write":
		return "", errors.New(`sandbox value "workspace-write" was removed; use Shuhari sandbox level "isolated"`)
	case "danger-full-access":
		return "", errors.New(`sandbox value "danger-full-access" was removed; use Shuhari sandbox level "unsandboxed" with --network=true and SHUHARI_I_UNDERSTAND_NO_CREDENTIAL_BOUNDARY=1`)
	default:
		return "", fmt.Errorf("unsupported Shuhari sandbox level %q; choose isolated, read-only, or unsandboxed", value)
	}
}

func ValidateSecurityPolicy(policy SecurityPolicy) error {
	if _, err := ParseSandboxLevel(string(policy.Level)); err != nil {
		return err
	}
	if policy.Level == SandboxUnsandboxed {
		if !policy.Network {
			return &UnsupportedSecurityPolicyError{Policy: policy, Reason: "network denial cannot be enforced; pass --network=true to acknowledge unrestricted egress"}
		}
		if os.Getenv(NoCredentialBoundaryAcknowledgementEnv) != "1" {
			return &UnsupportedSecurityPolicyError{Policy: policy, Reason: fmt.Sprintf("no credential boundary; set %s=1 only inside an isolated runner or container", NoCredentialBoundaryAcknowledgementEnv)}
		}
	}
	return nil
}

func ValidateSecurityResolution(policy SecurityPolicy, resolution SecurityResolution) error {
	if err := ValidateSecurityPolicy(policy); err != nil {
		return err
	}
	if resolution.SandboxLevel != policy.Level {
		return fmt.Errorf("invalid security resolution: sandbox level %q does not match requested %q", resolution.SandboxLevel, policy.Level)
	}
	wantNetwork := NetworkDenied
	if policy.Network {
		wantNetwork = NetworkAllowed
	}
	if resolution.NetworkAccess != wantNetwork {
		return fmt.Errorf("invalid security resolution: network access %q does not match requested %q", resolution.NetworkAccess, wantNetwork)
	}
	wantBoundary := CredentialBoundaryEnforced
	if policy.Level == SandboxUnsandboxed {
		wantBoundary = CredentialBoundaryNone
	}
	if resolution.CredentialBoundary != wantBoundary {
		return fmt.Errorf("invalid security resolution: credential boundary %q does not match required %q", resolution.CredentialBoundary, wantBoundary)
	}
	if resolution.Adapter.Name == "" || resolution.Adapter.NativeMode == "" {
		return errors.New("invalid security resolution: adapter name, native mode, and policy digest are required")
	}
	if !policyDigestPattern.MatchString(resolution.Adapter.PolicyDigest) {
		return errors.New("invalid security resolution: policy digest must match sha256:<lowercase-hex>")
	}
	return nil
}
