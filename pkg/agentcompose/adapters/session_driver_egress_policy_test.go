package adapters

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	"github.com/chaitin/agent-compose/pkg/egress"
	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/storage/sandboxstore"
)

// sequenceRuntimeProvider reports a different runtime on each call, so a test
// can state what creating a runtime reports and what starting the same runtime
// again reports.
type sequenceRuntimeProvider struct {
	infos []domain.SandboxVMInfo
	calls int
}

func (p *sequenceRuntimeProvider) ForDriver(string) (SandboxRuntime, error) {
	info := domain.SandboxVMInfo{}
	if p.calls < len(p.infos) {
		info = p.infos[p.calls]
	}
	p.calls++
	return fakeSessionRuntime{info: info}, nil
}

func (p *sequenceRuntimeProvider) ForSession(*domain.Sandbox) (SandboxRuntime, error) {
	return p.ForDriver("")
}

// TestSandboxDriverRecordsTheEgressPolicyItsRuntimeWasCreatedWith pins the fact
// drift reporting depends on: the record describes the *runtime*, so it is
// written when the runtime is created and left alone when an existing runtime is
// started again. Overwriting it with the policy now in force would report a
// sandbox as decided by a declaration that never reached it.
func TestSandboxDriverRecordsTheEgressPolicyItsRuntimeWasCreatedWith(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := &appconfig.Config{
		DataRoot:             root,
		SandboxRoot:          filepath.Join(root, "sandboxes"),
		RuntimeDriver:        driverpkg.RuntimeDriverBoxlite,
		BoxliteHome:          filepath.Join(root, "boxlite"),
		DefaultImage:         "guest:latest",
		GuestWorkspacePath:   "/workspace",
		JupyterGuestPort:     8888,
		JupyterProxyBasePath: "/agent-compose/session",
		SandboxStartTimeout:  2 * time.Second,
	}
	store, err := sandboxstore.NewWithConfig(config)
	if err != nil {
		t.Fatalf("NewWithConfig returned error: %v", err)
	}
	session, err := store.CreateSandbox(ctx, "egress drift", "", driverpkg.RuntimeDriverBoxlite, "guest:latest", "", domain.SandboxTypeManual, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	created := domain.SandboxEgressPolicy{Generation: 4242, Applied: string(egress.ApplicationOpen)}
	provider := &sequenceRuntimeProvider{}
	provider.infos = append(provider.infos, domain.SandboxVMInfo{BoxID: "container-1", EgressPolicy: created})
	provider.infos = append(provider.infos, domain.SandboxVMInfo{
		BoxID: "container-1",
		// The declaration changed while the sandbox was stopped: the runtime is
		// the same one, so this is what the engine would install for a *new*
		// runtime, not what the running one has.
		EgressPolicy: domain.SandboxEgressPolicy{Generation: 9001, Applied: string(egress.ApplicationMediated)},
	})
	driver := newTestSandboxDriver(config, store, nil, provider)

	if err := driver.StartSandboxVM(ctx, session); err != nil {
		t.Fatalf("first StartSandboxVM returned error: %v", err)
	}
	vmState, err := store.GetVMState(session.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.EgressPolicy != created {
		t.Fatalf("recorded egress policy = %+v, want the one the runtime was created with (%+v)", vmState.EgressPolicy, created)
	}

	if err := driver.StartSandboxVM(ctx, session); err != nil {
		t.Fatalf("second StartSandboxVM returned error: %v", err)
	}
	vmState, err = store.GetVMState(session.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.EgressPolicy != created {
		t.Fatalf("recorded egress policy after restarting an existing runtime = %+v, want it unchanged at %+v so the declaration that does not reach this sandbox stays visible", vmState.EgressPolicy, created)
	}
}

// TestSandboxDriverRecordsNothingWhenTheRuntimeReportsNoPolicy keeps an
// undeclared-and-unreported runtime distinguishable from a recorded one: an
// empty record is what makes a later start say it cannot establish whether the
// policy in force reaches that sandbox.
func TestSandboxDriverRecordsNothingWhenTheRuntimeReportsNoPolicy(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := &appconfig.Config{
		DataRoot:             root,
		SandboxRoot:          filepath.Join(root, "sandboxes"),
		RuntimeDriver:        driverpkg.RuntimeDriverBoxlite,
		BoxliteHome:          filepath.Join(root, "boxlite"),
		DefaultImage:         "guest:latest",
		GuestWorkspacePath:   "/workspace",
		JupyterGuestPort:     8888,
		JupyterProxyBasePath: "/agent-compose/session",
		SandboxStartTimeout:  2 * time.Second,
	}
	store, err := sandboxstore.NewWithConfig(config)
	if err != nil {
		t.Fatalf("NewWithConfig returned error: %v", err)
	}
	session, err := store.CreateSandbox(ctx, "no record", "", driverpkg.RuntimeDriverBoxlite, "guest:latest", "", domain.SandboxTypeManual, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	driver := newTestSandboxDriver(config, store, nil, fakeRuntimeProvider{runtime: fakeSessionRuntime{info: domain.SandboxVMInfo{BoxID: "container-1"}}})

	if err := driver.StartSandboxVM(ctx, session); err != nil {
		t.Fatalf("StartSandboxVM returned error: %v", err)
	}
	vmState, err := store.GetVMState(session.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.EgressPolicy.Recorded() {
		t.Fatalf("recorded egress policy = %+v, want no record for a runtime that reported none", vmState.EgressPolicy)
	}
}
