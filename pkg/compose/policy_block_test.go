package compose

import (
	"strings"
	"testing"

	domain "github.com/chaitin/agent-compose/pkg/model"
)

// The agent policy block was renamed from `sandbox` to `policy`. The deprecated
// key stays a working spelling: a project written before the rename keeps
// normalizing to the same policy, and therefore to the same canonical JSON and
// spec hash. Reading that key as absent would silently turn a declared policy
// into an undeclared one, which the engine treats as unrestricted.
func TestDeprecatedSandboxKeyNormalizesIdenticallyToPolicy(t *testing.T) {
	const declaration = `
      stopped_runtime_policy: retain
      network:
        default: deny
        allow:
          - host: api.github.com
            port: 443
            protocol: https
`
	policySpec := mustNormalizeCompose(t, `
name: policy-alias
agents:
  worker:
    provider: codex
    policy:
`+declaration, nil)
	legacySpec := mustNormalizeCompose(t, `
name: policy-alias
agents:
  worker:
    provider: codex
    sandbox:
`+declaration, nil)

	policyJSON, err := policySpec.MarshalCanonicalJSON(false)
	if err != nil {
		t.Fatalf("MarshalCanonicalJSON returned error: %v", err)
	}
	legacyJSON, err := legacySpec.MarshalCanonicalJSON(false)
	if err != nil {
		t.Fatalf("MarshalCanonicalJSON returned error: %v", err)
	}
	if string(policyJSON) != string(legacyJSON) {
		t.Fatalf("deprecated sandbox key normalized differently:\npolicy:  %s\nsandbox: %s", policyJSON, legacyJSON)
	}
	if !strings.Contains(string(legacyJSON), `"policy"`) || strings.Contains(string(legacyJSON), `"sandbox"`) {
		t.Fatalf("canonical JSON = %s, want the policy key rewritten and no deprecated key", legacyJSON)
	}
	if got, want := mustHash(t, legacySpec), mustHash(t, policySpec); got != want {
		t.Fatalf("hash = %s, want %s for the same declaration under the policy key", got, want)
	}
}

func TestPolicyBlockRejectsDeclaringBothKeys(t *testing.T) {
	_, err := Normalize(&ProjectSpec{Name: "project", Agents: map[string]AgentSpec{
		"worker": {
			Policy:  &SandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRemove},
			Sandbox: &SandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRetain},
		},
	}}, NormalizeOptions{})
	if err == nil || !strings.Contains(err.Error(), "agents.worker.sandbox") || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("Normalize error = %v, want a both-keys rejection naming the agent", err)
	}
}

// A declaration error must name the key the project actually used, so the
// message points at a line that exists in the file.
func TestPolicyBlockErrorPathNamesTheDeclaredKey(t *testing.T) {
	tests := []struct {
		name    string
		policy  *SandboxSpec
		legacy  *SandboxSpec
		wantErr string
	}{
		{
			name:    "policy key",
			policy:  &SandboxSpec{StoppedRuntimePolicy: "prune"},
			wantErr: "agents.worker.policy.stopped_runtime_policy",
		},
		{
			name:    "deprecated sandbox key",
			legacy:  &SandboxSpec{StoppedRuntimePolicy: "prune"},
			wantErr: "agents.worker.sandbox.stopped_runtime_policy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalize(&ProjectSpec{Name: "project", Agents: map[string]AgentSpec{
				"worker": {Policy: tt.policy, Sandbox: tt.legacy},
			}}, NormalizeOptions{})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Normalize error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestPolicyBlockAccessorsPreferPolicy(t *testing.T) {
	policy := &SandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRemove}
	legacy := &SandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRetain}

	if got := (AgentSpec{Policy: policy, Sandbox: legacy}).PolicyBlock(); got != policy {
		t.Fatalf("AgentSpec.PolicyBlock() = %#v, want the policy block", got)
	}
	if got := (AgentSpec{Sandbox: legacy}).PolicyBlock(); got != legacy {
		t.Fatalf("AgentSpec.PolicyBlock() = %#v, want the deprecated block", got)
	}
	if got := (AgentSpec{}).PolicyBlock(); got != nil {
		t.Fatalf("AgentSpec.PolicyBlock() = %#v, want nil for an undeclared policy", got)
	}

	normalizedPolicy := &NormalizedSandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRemove}
	normalizedLegacy := &NormalizedSandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRetain}
	if got := (NormalizedAgentSpec{Policy: normalizedPolicy, Sandbox: normalizedLegacy}).PolicyBlock(); got != normalizedPolicy {
		t.Fatalf("NormalizedAgentSpec.PolicyBlock() = %#v, want the policy block", got)
	}
	if got := (NormalizedAgentSpec{Sandbox: normalizedLegacy}).PolicyBlock(); got != normalizedLegacy {
		t.Fatalf("NormalizedAgentSpec.PolicyBlock() = %#v, want the deprecated block", got)
	}
	if got := (NormalizedAgentSpec{}).PolicyBlock(); got != nil {
		t.Fatalf("NormalizedAgentSpec.PolicyBlock() = %#v, want nil for an undeclared policy", got)
	}
}
