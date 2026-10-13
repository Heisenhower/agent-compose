package driver

import (
	"fmt"
	"strings"

	config "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/egress"
)

// This file owns the account of what a project declared about a sandbox's
// outbound network access and which mechanism will actually apply it. It exists
// because admission is the only moment an operator can act on the answer: a
// declaration that the engine will not enforce must be reported when the project
// is accepted, not discovered later from the absence of a restriction.
//
// The report is derived from the same two inputs the creation path uses — the
// declaration (egress.ReportNetworkPolicy compiles it first) and the driver's
// enforcement descriptor, swapping in the forward-proxy descriptor exactly when
// the engine will mediate — so an admission report cannot claim a mechanism the
// seam would not install.

// AgentNetworkPolicyReport is the auditable account of one agent's declared
// outbound network policy and the mechanism that will apply it.
type AgentNetworkPolicyReport struct {
	// Agent is the project-local agent name the declaration belongs to.
	Agent string
	// Driver is the resolved runtime driver the report was computed for.
	Driver string
	// Declared is false when the agent declared no network policy. Undeclared is
	// not deny (D3): egress stays unrestricted and there is nothing to enforce.
	Declared bool
	// Restricts is false when the declaration cannot refuse anything, because an
	// allow list only restricts under default: deny. It is false for an
	// undeclared policy too.
	Restricts bool
	// Default is the action applied to traffic no entry matches.
	Default egress.Action
	// Allow lists the declared allowances, including their address ranges.
	Allow []egress.DeclaredAllowance
	// Mediated reports whether the engine's own forward proxy will apply the
	// declaration rather than the driver.
	Mediated bool
	// Enforcement is the mechanism that will apply the declaration.
	Enforcement SandboxNetworkEnforcement
	// Enforced reports whether that mechanism applies the declaration.
	Enforced bool
	// Unapplied names the parts of the declaration the mechanism does not apply.
	// It is empty when the mechanism applies all of it.
	Unapplied []string
	// Warning states what the operator must know when the declaration will not be
	// enforced as written. It is empty when it will be.
	Warning string
}

// ReportAgentNetworkPolicy accounts for one agent's declared policy on one
// resolved driver. mediated reports whether the engine's forward proxy will be
// in this sandbox's traffic path; mode is the deployment's
// EGRESS_UNENFORCEABLE_POLICY, which decides what an unenforceable declaration
// means so the report can state the outcome instead of restating the knob.
//
// A nil declaration reports Declared false: the engine cannot restrict a sandbox
// that did not ask, so there is no mechanism to name and no gap to report.
func ReportAgentNetworkPolicy(agent, driver string, declaration *egress.NetworkDeclaration, mediated bool, mode config.EgressUnenforceablePolicy) (AgentNetworkPolicyReport, error) {
	resolved := resolveRuntimeDriver(driver)
	report := AgentNetworkPolicyReport{Agent: agent, Driver: resolved}
	if declaration == nil {
		return report, nil
	}
	// egress.ReportNetworkPolicy compiles the declaration first, so a report that
	// disagrees with the decision model is impossible.
	declared, err := egress.ReportNetworkPolicy(declaration, nil)
	if err != nil {
		return AgentNetworkPolicyReport{}, fmt.Errorf("report declared network policy for agent %q: %w", agent, err)
	}
	report.Declared = true
	report.Default = declared.Default
	report.Allow = declared.Allow
	if declaration.Default != egress.Deny {
		// Documented and deliberate: an allow list restricts nothing unless the
		// default refuses what it does not match.
		return report, nil
	}
	report.Restricts = true

	enforcement := SandboxNetworkEnforcementFor(resolved)
	if mediated {
		enforcement = SandboxEgressForwardProxyEnforcement(resolved)
		report.Mediated = true
	}
	report.Enforcement = enforcement

	// The engine never claims a limit it does not install: the parts this
	// mechanism cannot apply are named, including the address constraint the
	// creation path also refuses to present as applied.
	addresses := egress.AddressConstrainedAllowances(declaration.Allow)
	switch {
	case !enforcement.AppliesAllowEntries:
		report.Unapplied = append(report.Unapplied, "the declared allowances")
	case addresses > 0 && !enforcement.AppliesAllowedAddresses:
		report.Unapplied = append(report.Unapplied, "the addresses the declared allowances may resolve to")
	}
	if !enforcement.AppliesEngineEndpoints {
		report.Unapplied = append(report.Unapplied, "the engine's own sandbox endpoints")
	}

	if enforcement.Strength == SandboxEgressStrengthNone {
		report.Warning = fmt.Sprintf(
			"the engine will not enforce this declaration on driver %q: %s; %s",
			resolved, enforcement.Notes, unenforceableOutcome(mode),
		)
		return report, nil
	}
	if addresses > 0 && !enforcement.AppliesAllowedAddresses && mode == config.EgressUnenforceableRefuse {
		// The creation path refuses exactly this gap, so a report that answered
		// Enforced would describe a sandbox that will never start.
		report.Warning = fmt.Sprintf("%s does not apply %s; %s",
			enforcement.Mechanism, strings.Join(report.Unapplied, ", "), unenforceableOutcome(mode))
		return report, nil
	}
	report.Enforced = true
	if len(report.Unapplied) > 0 {
		report.Warning = fmt.Sprintf("%s applies the declaration, but not %s; %s",
			enforcement.Mechanism, strings.Join(report.Unapplied, ", "), enforcement.Notes)
	}
	return report, nil
}

// unenforceableOutcome states what the deployment's policy does with an
// unenforceable declaration, so the report answers "and then what" rather than
// leaving the operator to look up the knob.
func unenforceableOutcome(mode config.EgressUnenforceablePolicy) string {
	if mode == config.EgressUnenforceableRefuse {
		return "a sandbox that declares it is refused (" + config.EgressUnenforceablePolicyName + "=refuse)"
	}
	return "a sandbox that declares it starts with unrestricted egress (" + config.EgressUnenforceablePolicyName + "=" + string(config.EgressUnenforceableWarn) + ")"
}
