package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/llms"
)

// modelCatalogStore is the surface that projecting models.json needs: the write
// surface, and the default reference read twice around it so a projection that
// replaced a stored default can be reported.
type modelCatalogStore interface {
	ApplyModelCatalog(context.Context, llms.ModelCatalog) error
	DefaultLLMModelReference(context.Context) (string, string, bool, error)
}

// llmConfigStore is the startup surface that materializing LLM configuration
// needs: the write surfaces for models.json and the daemon environment, plus the
// catalog reads that report whether a default model exists afterwards.
type llmConfigStore interface {
	modelCatalogStore
	llms.DefaultConfigStore
	llms.CatalogStore
	// HasLLMProviders separates "nothing is configured" from "everything that is
	// configured is disabled", which is what the startup warning reports.
	HasLLMProviders(context.Context) (bool, error)
}

// loadLLMConfig materializes every configured LLM connection into the store
// before any agent resolves one.
//
// The daemon has two ways to be told about an upstream connection: models.json
// on disk, and the LLM_API_* / ANTHROPIC_* environment. Both are projected here,
// once, so that afterwards the catalog is the single source every resolution
// reads — a request path never writes configuration, and a connection cannot
// change while the process is running.
func loadLLMConfig(ctx context.Context, config *appconfig.Config, store llmConfigStore) error {
	if config == nil || store == nil {
		return fmt.Errorf("llm configuration and store are required")
	}
	replacedDefault, err := loadModelCatalog(ctx, config, store)
	if err != nil {
		return err
	}
	if replacedDefault != nil {
		// models.json is the daemon's own declaration, so it wins — but an
		// operator who chose a default through the LLM service must be able to
		// see that the file outranked the choice.
		slog.Warn("the default model in models.json replaced the stored default model",
			"previous_provider_id", replacedDefault.ProviderID, "previous_model_id", replacedDefault.ModelID)
	}
	if err := llms.ProjectDaemonLLMConfig(ctx, config, store); err != nil {
		conflict, ok := llms.AsDefaultConfigConflict(err)
		if !ok {
			return fmt.Errorf("project daemon llm environment: %w", err)
		}
		// The environment is a fallback, so another source naming the same id
		// wins. Report it, but never fail startup over it.
		slog.Warn("daemon llm environment was not applied because another configuration source owns the id",
			"kind", conflict.Kind, "id", conflict.ID, "owner_scope", conflict.Scope)
	}
	missing, err := defaultModelMissing(ctx, store)
	if err != nil {
		return err
	}
	if missing {
		slog.Warn("no default llm model is configured; agents that declare no model keep their own authentication. Configure a model in models.json or through the LLM RPC/UI")
		return nil
	}
	disabled, err := everyConnectionDisabled(ctx, store)
	if err != nil {
		return err
	}
	if disabled {
		slog.Warn("no llm connection is enabled, so runs that declare no model have nothing to route to; a models.json Provider without an apiKey is persisted disabled, so declare a credential or \"enabled\": true for a connection that needs none")
	}
	return nil
}

// defaultModelMissing reports whether a run that declares no model has nothing
// to fall back to. It is deliberately non-blocking: models can also be
// configured later through the LLM RPC, and an agent that declares its own
// upstream is unaffected.
func defaultModelMissing(ctx context.Context, store llms.CatalogStore) (bool, error) {
	catalog, err := llms.LoadCatalog(ctx, store)
	if err != nil {
		return false, fmt.Errorf("load llm catalog after projection: %w", err)
	}
	if strings.TrimSpace(catalog.DefaultModel()) != "" {
		return false, nil
	}
	// A deployment with no llm connection at all is not misconfigured: it never
	// asked for daemon-managed models, and warning about a default would be noise
	// on every start. The warning is for an operator who configured an upstream
	// but left the default unset, where a run that declares no model has nothing
	// to fall back to.
	connections, err := store.ListEnabledLLMProviders(ctx)
	if err != nil {
		return false, fmt.Errorf("list llm connections after projection: %w", err)
	}
	return len(connections) > 0, nil
}

// everyConnectionDisabled reports a deployment that configured at least one llm
// connection while every one of them is persisted disabled. A row alone routes
// nothing, so this is the state where a run that declares no model silently has no
// upstream, and the file or environment that produced it is the only clue.
//
// A deployment with no connection at all is not this state: it never asked for a
// daemon-managed model, and reporting it would warn on every start of a daemon that
// only runs agents declaring their own upstreams.
func everyConnectionDisabled(ctx context.Context, store llmConfigStore) (bool, error) {
	configured, err := store.HasLLMProviders(ctx)
	if err != nil {
		return false, fmt.Errorf("read whether llm connections are configured: %w", err)
	}
	if !configured {
		return false, nil
	}
	enabled, err := store.ListEnabledLLMProviders(ctx)
	if err != nil {
		return false, fmt.Errorf("list llm connections after projection: %w", err)
	}
	return len(enabled) == 0, nil
}

func loadModelCatalog(ctx context.Context, config *appconfig.Config, store modelCatalogStore) (*llms.ModelReference, error) {
	if config == nil || store == nil {
		return nil, fmt.Errorf("model catalog configuration and store are required")
	}
	path := filepath.Join(config.DataRoot, llms.ModelsCatalogFilename)
	catalog, err := llms.LoadModelCatalog(path, os.LookupEnv)
	if err != nil {
		return nil, err
	}
	// models.json declares its default explicitly, so projecting it replaces
	// whatever reference is stored — including one an operator chose through the
	// LLM service. Read the previous value so the caller can report an override
	// instead of silently outranking the choice the service accepted earlier.
	previousProviderID, previousModelID, hadPrevious, err := store.DefaultLLMModelReference(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the stored llm default before applying %s: %w", path, err)
	}
	if err := store.ApplyModelCatalog(ctx, catalog); err != nil {
		dropped, ok := llms.AsCatalogDefaultDropped(err)
		if !ok {
			return nil, fmt.Errorf("apply model catalog %s: %w", path, err)
		}
		// Every other declaration in the file is applied and the stored reference is
		// cleared, so the projection is reported rather than treated as a failed
		// write: refusing to start would take the whole daemon — cleanup, scheduler,
		// deletion recovery — down over one default the file cannot serve.
		slog.Warn("models.json default model was not applied",
			"declared", dropped.Declared, "stored", dropped.Stored, "reason", dropped.Reason)
		// A dropped reference was removed, not replaced, so the override report
		// below must not claim the file outranked the stored choice.
		return nil, nil
	}
	// A missing previous reference is not an override, and models.json that
	// declares no default deliberately leaves the stored reference in place, so
	// only a reference that changed is reported.
	if !hadPrevious {
		return nil, nil
	}
	providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the llm default after applying %s: %w", path, err)
	}
	if ok && providerID == previousProviderID && modelID == previousModelID {
		return nil, nil
	}
	return &llms.ModelReference{ProviderID: previousProviderID, ModelID: previousModelID}, nil
}
