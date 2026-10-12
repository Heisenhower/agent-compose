package adapters

import (
	"github.com/chaitin/agent-compose/pkg/egress"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

// This file owns the mapping between the egress fact recorded on a sandbox
// runtime (pkg/model, the persistence shape) and the egress package's domain
// value, so both directions stay in one place and an unknown stored value is
// handled explicitly rather than silently becoming a valid state.

// sandboxEgressPolicyRecord renders a domain egress fact as the persisted record.
func sandboxEgressPolicyRecord(policy egress.RuntimePolicy) domain.SandboxEgressPolicy {
	if !policy.Recorded() {
		return domain.SandboxEgressPolicy{}
	}
	return domain.SandboxEgressPolicy{
		Generation: uint64(policy.Generation),
		Applied:    string(policy.Application),
	}
}

// egressRuntimePolicyFromRecord reads the record back. A value this build does
// not know is reported as unrecorded: the engine then declines to claim that the
// policy in force reaches that sandbox, which is the safe direction.
func egressRuntimePolicyFromRecord(record domain.SandboxEgressPolicy) egress.RuntimePolicy {
	application := egress.Application(record.Applied)
	switch application {
	case egress.ApplicationOpen,
		egress.ApplicationDriverApplied,
		egress.ApplicationUnenforced,
		egress.ApplicationMediated:
		return egress.RuntimePolicy{Generation: egress.Generation(record.Generation), Application: application}
	default:
		return egress.RuntimePolicy{}
	}
}
