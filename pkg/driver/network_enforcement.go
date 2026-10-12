package driver

import (
	"errors"
	"fmt"
	"log/slog"

	config "github.com/chaitin/agent-compose/pkg/config"
)

// This file owns the honest strength of sandbox egress enforcement per driver
// and the gate that keeps a declared default-deny policy from silently
// degrading to unrestricted egress: it either reports the degradation or, when
// the deployment configures it, refuses the sandbox.
//
// The strength is a driver-local descriptor rather than a pkg/capmatrix type
// because pkg/driver cannot import pkg/capmatrix (pkg/llms imports pkg/driver).
// pkg/capmatrix consumes it through the egress_policy dimension's Mechanism and
// Observed fields, which is the seam SEC-3 wires into the runtime API.

// SandboxEgressStrength is how strongly a driver enforces a declared egress
// policy. It is reported alongside the driver capability facts, never assumed.
type SandboxEgressStrength string

const (
	// SandboxEgressStrengthNone means the driver applies no part of a declared
	// policy; egress stays as open as it is without one.
	SandboxEgressStrengthNone SandboxEgressStrength = "none"
	// SandboxEgressStrengthAllowList means the driver applies default deny plus
	// the ordered allowance list, including the engine-owned endpoints.
	SandboxEgressStrengthAllowList SandboxEgressStrength = "allowlist"
)

// SandboxNetworkEnforcement describes one runtime driver's egress enforcement.
type SandboxNetworkEnforcement struct {
	Driver    string
	Strength  SandboxEgressStrength
	Mechanism string
	// AppliesAllowEntries reports whether declared allowances reach the driver.
	AppliesAllowEntries bool
	// AppliesEngineEndpoints reports whether the engine-owned endpoints stay
	// reachable under default deny.
	AppliesEngineEndpoints bool
	// AppliesDenyDomains reports whether the declared deny domains reach the
	// driver's DNS resolution path.
	AppliesDenyDomains bool
	// Notes is the human-readable limit of what the driver applies, so a report
	// never presents a partial mechanism as complete.
	Notes string
}

// SandboxNetworkEnforcementFor returns the enforcement declaration for one
// runtime driver. It is pure and never probes a runtime.
func SandboxNetworkEnforcementFor(driver string) SandboxNetworkEnforcement {
	switch resolveRuntimeDriver(driver) {
	case RuntimeDriverMicrosandbox:
		return SandboxNetworkEnforcement{
			Driver:                 RuntimeDriverMicrosandbox,
			Strength:               SandboxEgressStrengthAllowList,
			Mechanism:              mechanismSDKNetworkPolicy,
			AppliesAllowEntries:    true,
			AppliesEngineEndpoints: true,
			AppliesDenyDomains:     true,
			Notes:                  "the SDK NetworkConfig applies ordered allow rules, a deny egress default, and the deny-domain list; the hypervisor's exact domain-suffix semantics still need a real KVM run",
		}
	case RuntimeDriverK8s:
		// A per-sandbox egress NetworkPolicy would deny all egress, including
		// the engine's OWN LLM facade and telemetry endpoints: NetworkPolicy is
		// L3/L4 and cannot express the declared allowances or those endpoints,
		// and the engine cannot even verify that the cluster CNI enforces
		// NetworkPolicy. Applying it would produce a healthy-looking sandbox
		// that can never call its model, so the engine refuses instead.
		return SandboxNetworkEnforcement{
			Driver:                 RuntimeDriverK8s,
			Strength:               SandboxEgressStrengthNone,
			Mechanism:              reasonNotConfigured,
			AppliesAllowEntries:    false,
			AppliesEngineEndpoints: false,
			AppliesDenyDomains:     false,
			Notes:                  "no egress filter is installed for this sandbox: a per-sandbox egress NetworkPolicy cannot express the declared DNS-named allowances or keep the engine's own LLM facade and telemetry endpoints reachable (NetworkPolicy is L3/L4 and cannot match DNS names), and the engine cannot verify that the cluster CNI would enforce it at all",
		}
	case RuntimeDriverDocker:
		// NetworkMode=none is a real outer deny, but it also severs the
		// engine's OWN endpoints: the sandbox reaches the daemon's LLM facade
		// and telemetry over host:port HTTP, and a loopback-only network
		// namespace cannot reach them. Applying it would produce a
		// healthy-looking sandbox that can never call its model, so the engine
		// refuses a declared default-deny instead.
		return SandboxNetworkEnforcement{
			Driver:                 RuntimeDriverDocker,
			Strength:               SandboxEgressStrengthNone,
			Mechanism:              reasonNotConfigured,
			AppliesAllowEntries:    false,
			AppliesEngineEndpoints: false,
			AppliesDenyDomains:     false,
			Notes:                  "no egress filter is installed for this sandbox: NetworkMode=none is a real outer deny but it would also deny the engine's own LLM facade and telemetry endpoints, leaving a healthy-looking sandbox that can never call its model, and applying the declared allowances while preserving those endpoints needs a per-sandbox netns or an egress proxy plus the deferred connect(2)/L7 mediator, which the base compose grants neither NET_ADMIN nor root for",
		}
	case RuntimeDriverBoxlite:
		return SandboxNetworkEnforcement{
			Driver:                 RuntimeDriverBoxlite,
			Strength:               SandboxEgressStrengthNone,
			Mechanism:              reasonNotConfigured,
			AppliesAllowEntries:    false,
			AppliesEngineEndpoints: false,
			AppliesDenyDomains:     false,
			Notes:                  "the driver sets network_enabled unconditionally and binds neither boxlite_options_add_network_allow nor boxlite_options_set_network_disabled, so a declared deny policy is not applied until the FFI binding is verified on a real KVM host",
		}
	default:
		return SandboxNetworkEnforcement{
			Driver:    resolveRuntimeDriver(driver),
			Strength:  SandboxEgressStrengthNone,
			Mechanism: reasonUnsupported,
			Notes:     "the driver has no egress enforcement surface",
		}
	}
}

// ErrSandboxNetworkEnforcementUnavailable reports a declared default-deny
// policy the selected driver cannot enforce. It is returned instead of
// starting a sandbox with unrestricted egress.
var ErrSandboxNetworkEnforcementUnavailable = errors.New("sandbox network enforcement unavailable")

// egressPolicyFacts derives the egress_policy capability declaration from the
// driver's actual enforcement strength, so the capability report can never
// claim more than the driver applies. pkg/capmatrix consumes these facts; the
// strength string travels in Observed because the capability contract has no
// separate strength field.
func egressPolicyFacts(driver string) RuntimeCapabilityDimensionFacts {
	enforcement := SandboxNetworkEnforcementFor(driver)
	facts := RuntimeCapabilityDimensionFacts{
		Dimension:       dimensionEgressPolicy,
		Observed:        fmt.Sprintf("strength=%s; %s", enforcement.Strength, enforcement.Notes),
		DefaultBehavior: "an undeclared network policy is not deny, so egress stays unrestricted (D3); when a sandbox declares default: deny the engine auto-allows its own runtime LLM facade and telemetry endpoints as non-overridable engine-side rules, and on a driver that cannot apply the declaration the sandbox reports the declaration as unenforced and runs, unless the deployment sets EGRESS_UNENFORCEABLE_POLICY=refuse, in which case the sandbox is refused instead",
	}
	if enforcement.Strength == SandboxEgressStrengthNone {
		facts.Mechanism = reasonNotConfigured
		return facts
	}
	facts.Enforced = true
	facts.Mechanism = enforcement.Mechanism
	return facts
}

// RequireSandboxNetworkEnforcement applies the configured policy to a sandbox
// that declares default-deny egress on a driver that cannot apply any part of
// it. A nil or permissive policy returns nil: an undeclared policy keeps today's
// behavior (D3), so this gate never tightens a sandbox that did not ask for it.
//
// A restriction the driver cannot deliver must not become an availability
// failure, so the default (warn) starts the sandbox and reports that the
// declaration is unenforced; refuse is opt-in for deployments that require the
// declaration to be real. Either way the sandbox is never presented as
// constrained when it is not.
func RequireSandboxNetworkEnforcement(driver, sandboxID string, policy *SandboxNetworkPolicy, mode config.EgressUnenforceablePolicy) error {
	if policy == nil {
		return nil
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("sandbox network policy: %w", err)
	}
	if !policy.DenyByDefault() {
		return nil
	}
	enforcement := SandboxNetworkEnforcementFor(driver)
	if enforcement.Strength != SandboxEgressStrengthNone {
		return nil
	}
	if mode == config.EgressUnenforceableRefuse {
		return fmt.Errorf("%w: driver %q cannot enforce the declared default-deny network policy; refusing to start the sandbox with unrestricted egress (%s)", ErrSandboxNetworkEnforcementUnavailable, enforcement.Driver, enforcement.Notes)
	}
	slog.Warn("declared network policy is not enforced",
		"sandbox_id", sandboxID,
		"driver", enforcement.Driver,
		"declared_default", "deny",
		"egress", "unrestricted",
		"reason", enforcement.Notes,
	)
	return nil
}
