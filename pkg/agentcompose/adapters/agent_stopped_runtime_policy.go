package adapters

import (
	"encoding/json"
	"strings"

	"github.com/chaitin/agent-compose/pkg/compose"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

// stoppedRuntimePolicyBlock is the stored shape of the agent policy block. Only
// the stopped-runtime policy is read here; the rest of the block rides along in
// the same payload for other readers.
type stoppedRuntimePolicyBlock struct {
	StoppedRuntimePolicy string `json:"stopped_runtime_policy"`
}

// stoppedRuntimePolicyFromAgentDefinition reads the agent's stopped-runtime
// policy out of AgentDefinition.ConfigJSON. `sandbox` is the deprecated spelling
// of the policy block and definitions written before the rename still carry it,
// so both keys are accepted; `policy` wins when both are present.
func stoppedRuntimePolicyFromAgentDefinition(definition *domain.AgentDefinition) string {
	if definition == nil {
		return domain.DefaultStoppedRuntimePolicy
	}
	var config struct {
		Policy  *stoppedRuntimePolicyBlock `json:"policy"`
		Sandbox *stoppedRuntimePolicyBlock `json:"sandbox"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(definition.ConfigJSON)), &config); err != nil {
		return domain.StoppedRuntimePolicyRetain
	}
	block := config.Policy
	if block == nil {
		block = config.Sandbox
	}
	if block == nil {
		return domain.DefaultStoppedRuntimePolicy
	}
	policy, err := compose.NormalizeStoppedRuntimePolicy(block.StoppedRuntimePolicy)
	if err != nil {
		return domain.StoppedRuntimePolicyRetain
	}
	return policy
}
