package compose

import (
	"strings"
	"testing"

	domain "github.com/chaitin/agent-compose/pkg/model"
)

func TestNormalizeStoppedRuntimePolicy(t *testing.T) {
	tests := []struct {
		value string
		want  string
		ok    bool
	}{
		{want: domain.StoppedRuntimePolicyRemove, ok: true},
		{value: " retain ", want: domain.StoppedRuntimePolicyRetain, ok: true},
		{value: "REMOVE", want: domain.StoppedRuntimePolicyRemove, ok: true},
		{value: "prune"},
	}
	for _, tt := range tests {
		got, err := NormalizeStoppedRuntimePolicy(tt.value)
		if (err == nil) != tt.ok || got != tt.want {
			t.Fatalf("NormalizeStoppedRuntimePolicy(%q) = %q, %v; want %q, ok=%v", tt.value, got, err, tt.want, tt.ok)
		}
	}
}

// The tests below cover the wiring rather than the rule: Parse must carry the
// declared policy into SandboxSpec, and Normalize must run it through
// NormalizeStoppedRuntimePolicy and report failures under the agent's field
// path. Neither is exercised by the unit test above.

func TestParseAgentStoppedRuntimePolicy(t *testing.T) {
	parsed, err := Parse([]byte("agents:\n  worker:\n    sandbox:\n      stopped_runtime_policy: remove\n"))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if parsed.Agents["worker"].PolicyBlock() == nil || parsed.Agents["worker"].PolicyBlock().StoppedRuntimePolicy != domain.StoppedRuntimePolicyRemove {
		t.Fatalf("parsed sandbox = %#v", parsed.Agents["worker"].PolicyBlock())
	}
}

func TestNormalizeAgentStoppedRuntimePolicy(t *testing.T) {
	for _, tt := range []struct {
		name       string
		sandbox    *SandboxSpec
		wantPolicy string
		wantNil    bool
	}{
		{name: "missing", wantNil: true},
		{name: "default", sandbox: &SandboxSpec{}, wantPolicy: domain.StoppedRuntimePolicyRemove},
		{name: "remove", sandbox: &SandboxSpec{StoppedRuntimePolicy: " remove "}, wantPolicy: domain.StoppedRuntimePolicyRemove},
	} {
		t.Run(tt.name, func(t *testing.T) {
			normalized, err := Normalize(&ProjectSpec{Name: "project", Agents: map[string]AgentSpec{"worker": {Sandbox: tt.sandbox}}}, NormalizeOptions{})
			if err != nil {
				t.Fatalf("Normalize returned error: %v", err)
			}
			got := normalized.Agents[0].PolicyBlock()
			if tt.wantNil {
				if got != nil {
					t.Fatalf("sandbox = %#v, want nil", got)
				}
				return
			}
			if got == nil || got.StoppedRuntimePolicy != tt.wantPolicy {
				t.Fatalf("sandbox = %#v, want policy %q", got, tt.wantPolicy)
			}
		})
	}
}

func TestNormalizeAgentRejectsUnknownStoppedRuntimePolicy(t *testing.T) {
	_, err := Normalize(&ProjectSpec{Name: "project", Agents: map[string]AgentSpec{"worker": {Sandbox: &SandboxSpec{StoppedRuntimePolicy: "prune"}}}}, NormalizeOptions{})
	if err == nil || !strings.Contains(err.Error(), "agents.worker.sandbox.stopped_runtime_policy") {
		t.Fatalf("Normalize error = %v, want stopped runtime policy path", err)
	}
}

func TestNormalizeAgentRejectsK8sStoppedRuntimeRetention(t *testing.T) {
	_, err := Normalize(&ProjectSpec{Name: "project", Agents: map[string]AgentSpec{
		"worker": {
			Driver:  &DriverSpec{K8s: &K8sDriverSpec{}},
			Sandbox: &SandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRetain},
		},
	}}, NormalizeOptions{})
	if err == nil || !strings.Contains(err.Error(), "agents.worker.sandbox.stopped_runtime_policy") || !strings.Contains(err.Error(), "k8s does not support retain") {
		t.Fatalf("Normalize error = %v, want k8s retain validation error", err)
	}
}

func TestNormalizeAgentAllowsK8sStoppedRuntimeRemoval(t *testing.T) {
	normalized, err := Normalize(&ProjectSpec{Name: "project", Agents: map[string]AgentSpec{
		"worker": {
			Driver:  &DriverSpec{K8s: &K8sDriverSpec{}},
			Sandbox: &SandboxSpec{StoppedRuntimePolicy: domain.StoppedRuntimePolicyRemove},
		},
	}}, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize returned error: %v", err)
	}
	if got := normalized.Agents[0].PolicyBlock().StoppedRuntimePolicy; got != domain.StoppedRuntimePolicyRemove {
		t.Fatalf("stopped runtime policy = %q, want %q", got, domain.StoppedRuntimePolicyRemove)
	}
}
