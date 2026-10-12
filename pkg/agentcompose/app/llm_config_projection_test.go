package app

import (
	"context"
	"errors"
	"testing"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/llms"
)

// A connection id another source owns must not fail startup: the environment is
// a bootstrap fallback, so the projection reports the conflict and the daemon
// keeps running with the operator's configuration.
func TestLoadLLMConfigToleratesEnvProjectionConflict(t *testing.T) {
	t.Setenv("LLM_API_KEY", "env-key")
	t.Setenv("LLM_MODEL", "env-model")
	store := &llmConfigStoreFake{upsertErr: &llms.DefaultConfigConflict{Kind: "provider", ID: "default", Scope: llms.ProviderScopeCatalog}}
	if err := loadLLMConfig(context.Background(), &appconfig.Config{DataRoot: t.TempDir()}, store); err != nil {
		t.Fatalf("loadLLMConfig error = %v, want the conflict tolerated", err)
	}
	if store.upserts != 1 {
		t.Fatalf("projection upserts = %d, want 1", store.upserts)
	}
}

// An unrelated projection failure is still fatal.
func TestLoadLLMConfigReturnsNonConflictProjectionError(t *testing.T) {
	t.Setenv("LLM_API_KEY", "env-key")
	t.Setenv("LLM_MODEL", "env-model")
	store := &llmConfigStoreFake{upsertErr: errors.New("disk full")}
	if err := loadLLMConfig(context.Background(), &appconfig.Config{DataRoot: t.TempDir()}, store); err == nil {
		t.Fatal("loadLLMConfig returned nil for a non-conflict projection error")
	}
}

func TestDefaultModelMissing(t *testing.T) {
	cases := []struct {
		name        string
		store       *llmConfigStoreFake
		wantMissing bool
	}{
		{
			name:        "no connections at all",
			store:       &llmConfigStoreFake{},
			wantMissing: true,
		},
		{
			name:        "catalog default reference resolves",
			store:       &llmConfigStoreFake{providers: []llms.Provider{{ID: "gateway", Enabled: true}}, hasDefault: true, defaultProvider: "gateway", defaultModel: "model"},
			wantMissing: false,
		},
		{
			name: "model flag with a single serving connection resolves",
			store: &llmConfigStoreFake{
				providers: []llms.Provider{{ID: "gateway", Enabled: true}},
				models:    []llms.Model{{ID: "model", Enabled: true, DefaultModel: true}},
				bindings:  []llms.ProviderModelBinding{{ProviderID: "gateway", ModelID: "model"}},
			},
			wantMissing: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missing, err := defaultModelMissing(context.Background(), tc.store)
			if err != nil {
				t.Fatalf("defaultModelMissing: %v", err)
			}
			if missing != tc.wantMissing {
				t.Fatalf("missing = %v, want %v", missing, tc.wantMissing)
			}
		})
	}
}

// models.json declares its default explicitly, so projecting it replaces whatever
// reference is stored — including one an operator chose through the LLM service.
// The caller reports that override, so the choice the service accepted does not
// silently stop being the effective default.
func TestLoadModelCatalogReportsAReplacedDefault(t *testing.T) {
	cases := []struct {
		name         string
		store        *llmConfigStoreFake
		wantReplaced *llms.ModelReference
	}{
		{
			name: "the file replaced a stored choice",
			store: &llmConfigStoreFake{
				hasDefault: true, defaultProvider: "service", defaultModel: "service-model",
				catalogDefault: &llms.ModelReference{ProviderID: "file", ModelID: "file-model"},
			},
			wantReplaced: &llms.ModelReference{ProviderID: "service", ModelID: "service-model"},
		},
		{
			name: "the file declares the stored choice",
			store: &llmConfigStoreFake{
				hasDefault: true, defaultProvider: "file", defaultModel: "file-model",
				catalogDefault: &llms.ModelReference{ProviderID: "file", ModelID: "file-model"},
			},
		},
		{
			name:  "nothing was stored to replace",
			store: &llmConfigStoreFake{catalogDefault: &llms.ModelReference{ProviderID: "file", ModelID: "file-model"}},
		},
		{
			name:  "the file declares no default",
			store: &llmConfigStoreFake{hasDefault: true, defaultProvider: "file", defaultModel: "file-model"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replaced, err := loadModelCatalog(context.Background(), &appconfig.Config{DataRoot: t.TempDir()}, tc.store)
			if err != nil {
				t.Fatalf("loadModelCatalog: %v", err)
			}
			if tc.wantReplaced == nil {
				if replaced != nil {
					t.Fatalf("replaced = %#v, want none", replaced)
				}
				return
			}
			if replaced == nil || *replaced != *tc.wantReplaced {
				t.Fatalf("replaced = %#v, want %#v", replaced, tc.wantReplaced)
			}
		})
	}
}

type llmConfigStoreFake struct {
	providers       []llms.Provider
	models          []llms.Model
	bindings        []llms.ProviderModelBinding
	hasDefault      bool
	defaultProvider string
	defaultModel    string
	upsertErr       error
	upserts         int
	// catalogDefault is what the fake's models.json projection writes, so a test
	// can observe what projecting the file did to the stored default.
	catalogDefault *llms.ModelReference
}

func (s *llmConfigStoreFake) ApplyModelCatalog(context.Context, llms.ModelCatalog) error {
	if s.catalogDefault != nil {
		s.defaultProvider, s.defaultModel, s.hasDefault = s.catalogDefault.ProviderID, s.catalogDefault.ModelID, true
	}
	return nil
}

func (s *llmConfigStoreFake) UpsertDefaultLLMConfig(context.Context, llms.Provider, llms.Model) error {
	s.upserts++
	return s.upsertErr
}

func (s *llmConfigStoreFake) ListEnabledLLMProviders(context.Context) ([]llms.Provider, error) {
	return s.providers, nil
}

func (s *llmConfigStoreFake) ListEnabledLLMModels(context.Context) ([]llms.Model, error) {
	return s.models, nil
}

func (s *llmConfigStoreFake) ListLLMProviderModelConfigs(context.Context) ([]llms.ProviderModelBinding, error) {
	return s.bindings, nil
}

func (s *llmConfigStoreFake) DefaultLLMModelReference(context.Context) (string, string, bool, error) {
	return s.defaultProvider, s.defaultModel, s.hasDefault, nil
}
