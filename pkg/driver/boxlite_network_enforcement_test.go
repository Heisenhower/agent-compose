//go:build linux && cgo && boxlitecgo

package driver

import (
	"context"
	"errors"
	"testing"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/egress"
)

// TestBoxliteEnsureSandboxRefusesDeclaredDefaultDeny pins that the BoxLite
// driver invokes the fail-closed gate before it creates any box. The driver
// reports no egress strength, so a deployment that configured
// EGRESS_UNENFORCEABLE_POLICY=refuse must get the machine-consumable reason
// rather than a sandbox with unrestricted egress - the exact gap that let a deny
// declaration silently start an open sandbox on BoxLite. The mode is set here
// because it is a deployment choice: the default starts the sandbox and reports.
func TestBoxliteEnsureSandboxRefusesDeclaredDefaultDeny(t *testing.T) {
	runtime := &cgoSandboxRuntime{config: &appconfig.Config{EgressUnenforceablePolicy: appconfig.EgressUnenforceableRefuse}}
	sandbox := &Sandbox{
		Summary:       SandboxSummary{ID: "sandbox-boxlite-deny"},
		NetworkPolicy: &SandboxNetworkPolicy{Default: egress.Deny},
	}

	_, err := runtime.EnsureSandbox(context.Background(), sandbox, VMState{}, ProxyState{})
	if !errors.Is(err, ErrSandboxNetworkEnforcementUnavailable) {
		t.Fatalf("EnsureSandbox() error = %v, want ErrSandboxNetworkEnforcementUnavailable", err)
	}
}
