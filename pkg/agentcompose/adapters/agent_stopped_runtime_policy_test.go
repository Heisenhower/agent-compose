package adapters

import (
	"testing"

	domain "github.com/chaitin/agent-compose/pkg/model"
)

func TestStoppedRuntimePolicyFromAgentDefinition(t *testing.T) {
	for _, tt := range []struct {
		name       string
		definition *domain.AgentDefinition
		want       string
	}{
		{name: "missing", want: domain.StoppedRuntimePolicyRemove},
		{name: "default", definition: &domain.AgentDefinition{ConfigJSON: `{}`}, want: domain.StoppedRuntimePolicyRemove},
		{name: "empty policy", definition: &domain.AgentDefinition{ConfigJSON: `{"policy":{}}`}, want: domain.StoppedRuntimePolicyRemove},
		{name: "retain", definition: &domain.AgentDefinition{ConfigJSON: `{"policy":{"stopped_runtime_policy":"retain"}}`}, want: domain.StoppedRuntimePolicyRetain},
		{name: "remove", definition: &domain.AgentDefinition{ConfigJSON: `{"policy":{"stopped_runtime_policy":"remove"}}`}, want: domain.StoppedRuntimePolicyRemove},
		{name: "invalid json", definition: &domain.AgentDefinition{ConfigJSON: `{`}, want: domain.StoppedRuntimePolicyRetain},
		// Agent definitions written before the policy block was renamed store it
		// under `sandbox`. Reading that key as absent would silently turn a
		// retained sandbox into a removed one.
		{name: "deprecated empty block", definition: &domain.AgentDefinition{ConfigJSON: `{"sandbox":{}}`}, want: domain.StoppedRuntimePolicyRemove},
		{name: "deprecated retain", definition: &domain.AgentDefinition{ConfigJSON: `{"sandbox":{"stopped_runtime_policy":"retain"}}`}, want: domain.StoppedRuntimePolicyRetain},
		{name: "deprecated remove", definition: &domain.AgentDefinition{ConfigJSON: `{"sandbox":{"stopped_runtime_policy":"remove"}}`}, want: domain.StoppedRuntimePolicyRemove},
		// A definition cannot legitimately carry both keys, having been written by
		// exactly one version; the current key is the one that wins.
		{
			name:       "current key wins over the deprecated key",
			definition: &domain.AgentDefinition{ConfigJSON: `{"policy":{"stopped_runtime_policy":"remove"},"sandbox":{"stopped_runtime_policy":"retain"}}`},
			want:       domain.StoppedRuntimePolicyRemove,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := stoppedRuntimePolicyFromAgentDefinition(tt.definition); got != tt.want {
				t.Fatalf("policy = %q, want %q", got, tt.want)
			}
		})
	}
}
