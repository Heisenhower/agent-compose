package adapters

import (
	"context"
	"errors"
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
	// ensureErrs makes the n-th ensure fail, so the start failure path is
	// observable without a real runtime.
	ensureErrs []error
	calls      int
}

func (p *sequenceRuntimeProvider) ForDriver(string) (SandboxRuntime, error) {
	info := domain.SandboxVMInfo{}
	if p.calls < len(p.infos) {
		info = p.infos[p.calls]
	}
	var ensureErr error
	if p.calls < len(p.ensureErrs) {
		ensureErr = p.ensureErrs[p.calls]
	}
	p.calls++
	return fakeSessionRuntime{info: info, ensureErr: ensureErr}, nil
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
	provider.infos = append(provider.infos, domain.SandboxVMInfo{
		BoxID: "container-1",
		// A mediated runtime is decided by the policy in force on every request,
		// so its record follows the declaration: the generation it is decided by
		// is the current one.
		EgressPolicy: domain.SandboxEgressPolicy{Generation: 9002, Applied: string(egress.ApplicationMediated)},
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

	// A mediated runtime is the exception: the proxy reads the policy per
	// request, so the declaration it is decided by is the current one even when
	// the runtime itself was not recreated.
	mediatedSession, err := store.CreateSandbox(ctx, "mediated", "", driverpkg.RuntimeDriverBoxlite, "guest:latest", "", domain.SandboxTypeManual, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	mediatedCreated := domain.SandboxEgressPolicy{Generation: 555, Applied: string(egress.ApplicationMediated)}
	edited := domain.SandboxEgressPolicy{Generation: 556, Applied: string(egress.ApplicationMediated)}
	provider.infos = []domain.SandboxVMInfo{{EgressPolicy: mediatedCreated}, {BoxID: "container-2", EgressPolicy: edited}}
	provider.calls = 0
	if err := driver.StartSandboxVM(ctx, mediatedSession); err != nil {
		t.Fatalf("mediated StartSandboxVM returned error: %v", err)
	}
	if err := driver.StartSandboxVM(ctx, mediatedSession); err != nil {
		t.Fatalf("mediated restart returned error: %v", err)
	}
	vmState, err = store.GetVMState(mediatedSession.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.EgressPolicy != edited {
		t.Fatalf("recorded egress policy for a mediated runtime = %+v, want the current one (%+v)", vmState.EgressPolicy, edited)
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

// TestSandboxDriverKeepsAnEmptyRecordWhenAReusedRuntimeCarriesNone is the
// regression for a start that finds no record and does not create the runtime:
// the policy resolved now must not be written into the record. A runtime created
// before the engine kept this record, or by a driver that cannot report
// creation, would otherwise be reported as isolated by a declaration that never
// reached it, which is the one direction the record must never take.
func TestSandboxDriverKeepsAnEmptyRecordWhenAReusedRuntimeCarriesNone(t *testing.T) {
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
	session, err := store.CreateSandbox(ctx, "empty record", "", driverpkg.RuntimeDriverBoxlite, "guest:latest", "", domain.SandboxTypeManual, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}
	provider := &sequenceRuntimeProvider{}
	// The first start creates a runtime that reports no policy, so nothing is
	// recorded for it.
	provider.infos = append(provider.infos, domain.SandboxVMInfo{BoxID: "container-1"})
	// The second start reuses that runtime (the driver does not report creating
	// it, and the sandbox already started) while the declaration now resolves a
	// policy. The record has to stay empty.
	provider.infos = append(provider.infos, domain.SandboxVMInfo{
		BoxID:        "container-1",
		EgressPolicy: domain.SandboxEgressPolicy{Generation: 777, Applied: string(egress.ApplicationDriverApplied)},
	})
	driver := newTestSandboxDriver(config, store, nil, provider)

	if err := driver.StartSandboxVM(ctx, session); err != nil {
		t.Fatalf("first StartSandboxVM returned error: %v", err)
	}
	vmState, err := store.GetVMState(session.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.EgressPolicy.Recorded() {
		t.Fatalf("recorded egress policy = %+v, want no record for a runtime that reported none", vmState.EgressPolicy)
	}

	if err := driver.StartSandboxVM(ctx, session); err != nil {
		t.Fatalf("second StartSandboxVM returned error: %v", err)
	}
	vmState, err = store.GetVMState(session.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.EgressPolicy.Recorded() {
		t.Fatalf("recorded egress policy after reusing a runtime with no record = %+v, want it left empty so the start reports that the runtime cannot be accounted for", vmState.EgressPolicy)
	}
}

// TestSandboxDriverKeepsTheRecordWhenAStartFails is the regression for a start
// that fails after the runtime was asked to start: the start-attempt fence and
// the failure path both persist the driver-boundary projection, which does not
// carry the egress record, so the record has to be restored before either save.
// Losing it would report a running runtime as unaccounted for.
func TestSandboxDriverKeepsTheRecordWhenAStartFails(t *testing.T) {
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
	session, err := store.CreateSandbox(ctx, "failed start", "", driverpkg.RuntimeDriverBoxlite, "guest:latest", "", domain.SandboxTypeManual, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateSandbox returned error: %v", err)
	}

	created := domain.SandboxEgressPolicy{Generation: 4242, Applied: string(egress.ApplicationOpen)}
	provider := &sequenceRuntimeProvider{}
	provider.infos = append(provider.infos, domain.SandboxVMInfo{BoxID: "container-1", EgressPolicy: created})
	provider.infos = append(provider.infos, domain.SandboxVMInfo{BoxID: "container-1"})
	provider.ensureErrs = append(provider.ensureErrs, nil)
	provider.ensureErrs = append(provider.ensureErrs, errors.New("the runtime refused to start"))
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

	if err := driver.StartSandboxVM(ctx, session); err == nil {
		t.Fatal("second StartSandboxVM returned no error, want the runtime failure")
	}
	vmState, err = store.GetVMState(session.Summary.ID)
	if err != nil {
		t.Fatalf("GetVMState returned error: %v", err)
	}
	if vmState.EgressPolicy != created {
		t.Fatalf("recorded egress policy after a failed start = %+v, want it preserved at %+v", vmState.EgressPolicy, created)
	}
}
