package driver

import (
	"strings"
	"testing"

	config "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/egress"
)

func TestReportAgentNetworkPolicyUndeclaredIsNotDeny(t *testing.T) {
	report, err := ReportAgentNetworkPolicy("worker", RuntimeDriverDocker, nil, false, config.EgressUnenforceableWarn)
	if err != nil {
		t.Fatalf("ReportAgentNetworkPolicy() error = %v", err)
	}
	if report.Declared || report.Restricts || report.Enforced {
		t.Fatalf("report = %+v, want an undeclared policy that restricts nothing", report)
	}
	if report.Warning != "" {
		t.Fatalf("Warning = %q, want empty for an undeclared policy", report.Warning)
	}
	if report.Driver != RuntimeDriverDocker || report.Agent != "worker" {
		t.Fatalf("report identity = %+v, want agent worker on docker", report)
	}
}

func TestReportAgentNetworkPolicyAllowListWithoutDenyDoesNotRestrict(t *testing.T) {
	declaration := &egress.NetworkDeclaration{
		Default: egress.Allow,
		Allow:   []egress.AllowEntry{{Host: "api.example.com", Port: 443, Protocol: egress.ProtocolHTTPS}},
	}
	report, err := ReportAgentNetworkPolicy("worker", RuntimeDriverDocker, declaration, false, config.EgressUnenforceableWarn)
	if err != nil {
		t.Fatalf("ReportAgentNetworkPolicy() error = %v", err)
	}
	if !report.Declared || report.Restricts || report.Enforced {
		t.Fatalf("report = %+v, want a declared allow-all policy that restricts nothing", report)
	}
	if report.Warning != "" {
		t.Fatalf("Warning = %q, want empty: the declaration is delivered, it just does not restrict", report.Warning)
	}
	if len(report.Allow) != 1 || report.Allow[0].Endpoint != "api.example.com:443/https" || !report.Allow[0].Inspectable {
		t.Fatalf("Allow = %+v, want the declared https allowance reported as inspectable", report.Allow)
	}
}

func TestReportAgentNetworkPolicyMediatedDenyIsEnforced(t *testing.T) {
	declaration := &egress.NetworkDeclaration{
		Default: egress.Deny,
		Allow: []egress.AllowEntry{{
			Host: "api.example.com", Port: 443, Protocol: egress.ProtocolHTTPS,
			AllowedIPs: []string{"203.0.113.0/24"},
		}},
	}
	report, err := ReportAgentNetworkPolicy("worker", RuntimeDriverDocker, declaration, true, config.EgressUnenforceableWarn)
	if err != nil {
		t.Fatalf("ReportAgentNetworkPolicy() error = %v", err)
	}
	if !report.Restricts || !report.Enforced || !report.Mediated {
		t.Fatalf("report = %+v, want an enforced, mediated default-deny policy", report)
	}
	if report.Enforcement.Mechanism != mechanismForwardProxy || report.Enforcement.Strength != SandboxEgressStrengthAllowList {
		t.Fatalf("Enforcement = %+v, want the forward proxy at allowlist strength", report.Enforcement)
	}
	if len(report.Unapplied) != 0 || report.Warning != "" {
		t.Fatalf("report = %+v, want nothing unapplied: the proxy resolves the destination itself", report)
	}
	if len(report.Allow) != 1 || len(report.Allow[0].AllowedIPs) != 1 || report.Allow[0].AllowedIPs[0] != "203.0.113.0/24" {
		t.Fatalf("Allow = %+v, want the declared address range reported", report.Allow)
	}
}

func TestReportAgentNetworkPolicyNamesWhatTheMechanismLeavesUnapplied(t *testing.T) {
	tests := []struct {
		name         string
		driver       string
		mediated     bool
		mode         config.EgressUnenforceablePolicy
		declaration  *egress.NetworkDeclaration
		wantEnforced bool
		wantWarning  []string
	}{
		{
			name:   "a driver without an egress surface enforces nothing",
			driver: RuntimeDriverDocker,
			mode:   config.EgressUnenforceableWarn,
			wantWarning: []string{
				"will not enforce this declaration on driver \"docker\"",
				"no egress filter is installed by this driver",
				"EGRESS_UNENFORCEABLE_POLICY=warn",
			},
		},
		{
			name:   "refuse mode states the refusal it would produce",
			driver: RuntimeDriverK8s,
			mode:   config.EgressUnenforceableRefuse,
			wantWarning: []string{
				"EGRESS_UNENFORCEABLE_POLICY=refuse",
			},
		},
		{
			name:         "a driver that applies destinations but not addresses says so",
			driver:       RuntimeDriverMicrosandbox,
			mode:         config.EgressUnenforceableWarn,
			wantEnforced: true,
			wantWarning: []string{
				"does not constrain which addresses",
			},
		},
		{
			// The creation path refuses exactly this gap in refuse mode, so the
			// report must not answer Enforced for a sandbox that never starts.
			name:         "refuse mode does not present a refused declaration as enforced",
			driver:       RuntimeDriverMicrosandbox,
			mode:         config.EgressUnenforceableRefuse,
			wantEnforced: false,
			wantWarning: []string{
				"EGRESS_UNENFORCEABLE_POLICY=refuse",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			declaration := &egress.NetworkDeclaration{
				Default: egress.Deny,
				Allow: []egress.AllowEntry{{
					Host: "api.example.com", Port: 443, Protocol: egress.ProtocolHTTPS,
					AllowedIPs: []string{"203.0.113.0/24"},
				}},
			}
			report, err := ReportAgentNetworkPolicy("worker", test.driver, declaration, test.mediated, test.mode)
			if err != nil {
				t.Fatalf("ReportAgentNetworkPolicy() error = %v", err)
			}
			if report.Enforced != test.wantEnforced {
				t.Fatalf("Enforced = %v, want %v (%s)", report.Enforced, test.wantEnforced, report.Warning)
			}
			if len(report.Unapplied) == 0 {
				t.Fatalf("Unapplied is empty, want the parts the mechanism leaves: %+v", report)
			}
			for _, want := range test.wantWarning {
				if !strings.Contains(report.Warning, want) {
					t.Fatalf("Warning = %q, want it to contain %q", report.Warning, want)
				}
			}
		})
	}
}

// TestReportAgentNetworkPolicyRefusesAnInvalidDeclaration pins that the report
// cannot describe a policy the decision model would reject.
func TestReportAgentNetworkPolicyRefusesAnInvalidDeclaration(t *testing.T) {
	declaration := &egress.NetworkDeclaration{
		Default: egress.Deny,
		Allow:   []egress.AllowEntry{{Host: "bad host", Port: 443, Protocol: egress.ProtocolHTTPS}},
	}
	if _, err := ReportAgentNetworkPolicy("worker", RuntimeDriverDocker, declaration, true, config.EgressUnenforceableWarn); err == nil {
		t.Fatal("ReportAgentNetworkPolicy() error = nil, want a validation error")
	}
}
