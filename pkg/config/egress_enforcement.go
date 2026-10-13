package config

import (
	"fmt"
	"os"
	"strings"
)

// EgressUnenforceablePolicy selects what the engine does when a sandbox declares
// an outbound network policy the selected runtime driver cannot apply.
//
// It exists because a restriction the platform cannot deliver must not become an
// availability failure: making the agent unrunnable turns a security declaration
// into an outage, and it happens while a sandbox is created rather than when the
// project is accepted. The declaration is never silently dropped either — the
// engine logs and reports a sandbox it could not constrain.
type EgressUnenforceablePolicy string

const (
	// EgressUnenforceableWarn is the default. The sandbox starts, and the engine
	// logs that the declared policy is unenforced, so a declaration the driver
	// cannot apply is visible instead of being presented as a control.
	EgressUnenforceableWarn EgressUnenforceablePolicy = "warn"
	// EgressUnenforceableRefuse fails the sandbox instead of running it with
	// wider egress than declared. It is for deployments that require the
	// declaration to be real, and it is opt-in.
	EgressUnenforceableRefuse EgressUnenforceablePolicy = "refuse"
)

// EgressUnenforceablePolicyName is the environment variable that selects the
// policy. It is a deployment knob: project content cannot change it.
const EgressUnenforceablePolicyName = "EGRESS_UNENFORCEABLE_POLICY"

// ParseEgressUnenforceablePolicy accepts the configured value, treating an
// unset variable as the default. Anything else is rejected at startup rather
// than silently resolved to a default that changes enforcement.
func ParseEgressUnenforceablePolicy(raw string) (EgressUnenforceablePolicy, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(EgressUnenforceableWarn):
		return EgressUnenforceableWarn, nil
	case string(EgressUnenforceableRefuse):
		return EgressUnenforceableRefuse, nil
	default:
		return "", fmt.Errorf("expected %q or %q, got %q", EgressUnenforceableWarn, EgressUnenforceableRefuse, raw)
	}
}

func loadEgressUnenforceablePolicy() (EgressUnenforceablePolicy, error) {
	policy, err := ParseEgressUnenforceablePolicy(os.Getenv(EgressUnenforceablePolicyName))
	if err != nil {
		return "", fmt.Errorf("%s: %w", EgressUnenforceablePolicyName, err)
	}
	return policy, nil
}
