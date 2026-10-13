package adapters

import (
	"github.com/chaitin/agent-compose/pkg/capmatrix"
	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/storage/configstore"
	"github.com/chaitin/agent-compose/pkg/storage/sandboxstore"
)

// newTestSandboxDriver builds a SandboxDriver with an empty capability
// snapshot, which is the default path: no declaration means no requirement, so
// the fail-closed pre-flight is satisfied without tightening anything. Tests
// that need a real decision build a snapshot explicitly.
func newTestSandboxDriver(config *appconfig.Config, store *sandboxstore.Store, configDB *configstore.ConfigStore, runtimes RuntimeProvider) *SandboxDriver {
	return NewSandboxDriver(SandboxDriverDeps{
		Config:       config,
		Store:        store,
		ConfigDB:     configDB,
		Runtimes:     runtimes,
		Capabilities: capmatrix.Snapshot{},
	})
}
