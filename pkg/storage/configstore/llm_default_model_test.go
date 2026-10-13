package configstore

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chaitin/agent-compose/pkg/llms"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

func TestIntegrationDefaultModelLifecycle(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := FromDB(newMemoryDB(t))
	if err := store.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := "gateway-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "gateway", BaseURL: "https://gateway.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &key,
		Models: declaredModels(llms.ModelSpec{ID: "gpt-4o"}),
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, _, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
		t.Fatalf("default before any write ok=%v err=%v, want none", ok, err)
	}
	if _, err := resolveCatalogModel(ctx, store, ""); !errors.Is(err, llms.ErrNoModel) {
		t.Fatalf("resolve without a default error = %v, want ErrNoModel", err)
	}

	// A literal model the connection does not enumerate is still a valid default:
	// model ids are opaque, and the connection forwards them as written.
	stored, err := store.SetDefaultLLMModel(ctx, llms.ModelReference{ProviderID: "gateway", ModelID: "gpt-5-preview"})
	if err != nil {
		t.Fatalf("set default: %v", err)
	}
	if stored.ProviderID != "gateway" || stored.ModelID != "gpt-5-preview" {
		t.Fatalf("stored default = %#v", stored)
	}
	target, err := resolveCatalogModel(ctx, store, "")
	if err != nil {
		t.Fatalf("resolve the stored default: %v", err)
	}
	if target.Provider.ID != "gateway" || target.Model.ID != "gpt-5-preview" {
		t.Fatalf("default target = %#v", target)
	}

	// A models.json that declares no default must not wipe the operator's
	// choice, which is what makes the reference usable across restarts.
	if err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{Providers: map[string]llms.CatalogProvider{}}); err != nil {
		t.Fatalf("apply an empty catalog: %v", err)
	}
	if providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx); err != nil || !ok || providerID != "gateway" || modelID != "gpt-5-preview" {
		t.Fatalf("default after an empty catalog = %q/%q ok=%v err=%v, want preserved", providerID, modelID, ok, err)
	}

	// A models.json that does declare a default has the last word.
	baseURL, protocol, apiKey := "https://catalog.example/v1", llms.APIProtocolResponses, "catalog-key"
	if err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{
		Default: "catalog/catalog-model",
		Providers: map[string]llms.CatalogProvider{
			"catalog": {BaseURL: &baseURL, Protocol: &protocol, APIKey: &apiKey},
		},
	}); err != nil {
		t.Fatalf("apply a declared default: %v", err)
	}
	if providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx); err != nil || !ok || providerID != "catalog" || modelID != "catalog-model" {
		t.Fatalf("default after a declared catalog default = %q/%q ok=%v err=%v", providerID, modelID, ok, err)
	}
	// The file's reference replaced the stored one, which was the only thing
	// keeping that literal model's identity alive.
	assertModelIdentityPresence(t, ctx, store, "gpt-5-preview", false)
	assertModelIdentityPresence(t, ctx, store, "catalog-model", true)

	if err := store.ClearDefaultLLMModel(ctx); err != nil {
		t.Fatalf("clear default: %v", err)
	}
	if providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
		t.Fatalf("default after clearing = %q/%q ok=%v err=%v, want none", providerID, modelID, ok, err)
	}
	if _, err := resolveCatalogModel(ctx, store, ""); !errors.Is(err, llms.ErrNoModel) {
		t.Fatalf("resolve after clearing error = %v, want ErrNoModel", err)
	}
}

func TestIntegrationSetDefaultModelRejectsUnusableReferences(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := FromDB(newMemoryDB(t))
	if err := store.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := "gateway-key"
	disabled := false
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "gateway", BaseURL: "https://gateway.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &key, Enabled: &disabled,
	}); err != nil {
		t.Fatalf("create disabled provider: %v", err)
	}
	cases := []struct {
		name      string
		reference llms.ModelReference
		want      error
	}{
		{name: "missing provider", reference: llms.ModelReference{ProviderID: "absent", ModelID: "gpt-4o"}, want: domain.ErrNotFound},
		{name: "missing model", reference: llms.ModelReference{ProviderID: "gateway"}, want: domain.ErrInvalidArgument},
		{name: "missing provider id", reference: llms.ModelReference{ModelID: "gpt-4o"}, want: domain.ErrInvalidArgument},
		{name: "disabled provider", reference: llms.ModelReference{ProviderID: "gateway", ModelID: "gpt-4o"}, want: domain.ErrFailedPrecondition},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.SetDefaultLLMModel(ctx, tc.reference); !errors.Is(err, tc.want) {
				t.Fatalf("SetDefaultLLMModel error = %v, want %v", err, tc.want)
			}
			if _, _, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
				t.Fatalf("default after a rejected write ok=%v err=%v, want none", ok, err)
			}
		})
	}
}

// A declared connection carries one sandbox's own credential and is reachable
// only by the run that declared it. Making it the daemon default would offer that
// credential to every run that names the model, so the write is refused.
func TestIntegrationSetDefaultModelRejectsADeclaredConnection(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := FromDB(newMemoryDB(t))
	if err := store.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	declaredID := llms.DeclaredConnectionPrefix + "sandbox-1:openai:digest"
	if err := store.UpsertDeclaredConnection(ctx, llms.Provider{
		ID: declaredID, ProviderType: llms.ProviderFamilyOpenAI, DefaultWireAPI: llms.APIProtocolResponses,
		BaseURL: "https://declared.example/v1", APIKey: "declared-key",
	}); err != nil {
		t.Fatalf("store declared connection: %v", err)
	}
	if _, err := store.SetDefaultLLMModel(ctx, llms.ModelReference{ProviderID: declaredID, ModelID: "gpt-4o"}); !errors.Is(err, domain.ErrInvalidArgument) {
		t.Fatalf("SetDefaultLLMModel for a declared connection error = %v, want invalid argument", err)
	}
	if _, _, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
		t.Fatalf("default after a rejected declared reference ok=%v err=%v, want none", ok, err)
	}
}

// Deleting or disabling the connection a stored default names removes the
// reference: resolution treats it as absent, so leaving the row would make
// GetDefaultModel report a default that no run receives. A models.json
// projection, by contrast, deliberately keeps the row (see
// TestIntegrationApplyEmptyModelCatalogClearsCatalogOwnedStateButKeepsTheStoredDefault).
func TestIntegrationDefaultModelReferenceDoesNotSurviveItsConnection(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := FromDB(newMemoryDB(t))
	if err := store.InitSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := "gateway-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "gateway", BaseURL: "https://gateway.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &key,
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := store.SetDefaultLLMModel(ctx, llms.ModelReference{ProviderID: "gateway", ModelID: "gpt-4o"}); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if _, _, ok, err := store.DefaultLLMModelReference(ctx); err != nil || !ok {
		t.Fatalf("default after set ok=%v err=%v, want a reference", ok, err)
	}

	disabled := false
	if _, err := store.UpdateLLMProvider(ctx, llms.ProviderReplacement{ID: "gateway", Enabled: &disabled}); err != nil {
		t.Fatalf("disable provider: %v", err)
	}
	if providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
		t.Fatalf("default of a disabled connection = %q/%q ok=%v err=%v, want none", providerID, modelID, ok, err)
	}
	if _, err := resolveCatalogModel(ctx, store, ""); !errors.Is(err, llms.ErrNoModel) {
		t.Fatalf("resolve after disabling the default connection error = %v, want ErrNoModel", err)
	}

	enabled := true
	if _, err := store.UpdateLLMProvider(ctx, llms.ProviderReplacement{ID: "gateway", Enabled: &enabled}); err != nil {
		t.Fatalf("re-enable provider: %v", err)
	}
	if _, _, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
		t.Fatalf("default after re-enabling ok=%v err=%v, want the reference removed with the disable", ok, err)
	}

	// The same rule applies to a provider the operator deletes outright.
	if _, err := store.SetDefaultLLMModel(ctx, llms.ModelReference{ProviderID: "gateway", ModelID: "gpt-4o"}); err != nil {
		t.Fatalf("set default again: %v", err)
	}
	if err := store.DeleteLLMProvider(ctx, "gateway"); err != nil {
		t.Fatalf("delete provider: %v", err)
	}
	if providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
		t.Fatalf("default after deleting the connection = %q/%q ok=%v err=%v, want none", providerID, modelID, ok, err)
	}
	if _, err := resolveCatalogModel(ctx, store, ""); !errors.Is(err, llms.ErrNoModel) {
		t.Fatalf("resolve after deleting the default connection error = %v, want ErrNoModel", err)
	}
}

// Disabling a connection clears the default reference that names it, but the
// model identity only that reference kept alive was left behind: the identity
// sweep runs inside replaceProviderModels, which an update carrying no model set
// skips, and the reference was still stored when that sweep ran. The leftover
// (scope=api, enabled=1) then claimed the model id as a foreign owner, so the
// environment bootstrap yielded its whole projection to a model nothing served.
func TestIntegrationDisablingAConnectionSweepsTheIdentityItsDefaultKeptAlive(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	key := "gateway-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "gateway", BaseURL: "https://gateway.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &key,
		Models: declaredModels(llms.ModelSpec{ID: "gpt-4o"}),
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	// A literal the connection does not enumerate: only the default reference
	// keeps its identity row alive.
	if _, err := store.SetDefaultLLMModel(ctx, llms.ModelReference{ProviderID: "gateway", ModelID: "gpt-5-preview"}); err != nil {
		t.Fatalf("set default: %v", err)
	}
	assertModelIdentityPresence(t, ctx, store, "gpt-5-preview", true)

	disabled := false
	if _, err := store.UpdateLLMProvider(ctx, llms.ProviderReplacement{ID: "gateway", Enabled: &disabled}); err != nil {
		t.Fatalf("disable provider: %v", err)
	}
	if providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx); err != nil || ok {
		t.Fatalf("default of a disabled connection = %q/%q ok=%v err=%v, want none", providerID, modelID, ok, err)
	}
	assertModelIdentityPresence(t, ctx, store, "gpt-5-preview", false)
	// A model the connection still binds must survive the sweep.
	assertModelIdentityPresence(t, ctx, store, "gpt-4o", true)
}

// Clearing the effective default is the RPC counterpart of disabling the
// connection that serves it: the identity a literal default created is kept alive
// by the reference alone, so removing the reference must sweep it in the same
// transaction.
func TestIntegrationClearingTheDefaultSweepsTheIdentityItKeptAlive(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	key := "gateway-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "gateway", BaseURL: "https://gateway.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &key,
		Models: declaredModels(llms.ModelSpec{ID: "gpt-4o"}),
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := store.SetDefaultLLMModel(ctx, llms.ModelReference{ProviderID: "gateway", ModelID: "gpt-5-preview"}); err != nil {
		t.Fatalf("set default: %v", err)
	}
	assertModelIdentityPresence(t, ctx, store, "gpt-5-preview", true)

	if err := store.ClearDefaultLLMModel(ctx); err != nil {
		t.Fatalf("clear default: %v", err)
	}
	assertModelIdentityPresence(t, ctx, store, "gpt-5-preview", false)
	assertModelIdentityPresence(t, ctx, store, "gpt-4o", true)
}

// A model identity row used to outlive the connection that declared it. The row
// still claimed the id for the environment bootstrap, which yields its whole
// projection to any foreign-scope owner, so a model an operator had dropped from
// an API connection could silently disable LLM_MODEL for every later daemon.
func TestIntegrationProviderWritesSweepUnreferencedModelIdentities(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	firstKey := "first-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "first", BaseURL: "https://first.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &firstKey,
		Models: declaredModels(llms.ModelSpec{ID: "shared"}, llms.ModelSpec{ID: "only-first"}),
	}); err != nil {
		t.Fatalf("create first provider: %v", err)
	}
	secondKey := "second-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "second", BaseURL: "https://second.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &secondKey,
		Models: declaredModels(llms.ModelSpec{ID: "shared"}),
	}); err != nil {
		t.Fatalf("create second provider: %v", err)
	}

	if err := store.DeleteLLMProvider(ctx, "first"); err != nil {
		t.Fatalf("delete first provider: %v", err)
	}
	assertModelIdentityPresence(t, ctx, store, "only-first", false)
	// Another connection still serves this model, so its identity must survive.
	assertModelIdentityPresence(t, ctx, store, "shared", true)

	// Dropping a model from the declared set removes its identity too.
	if _, err := store.UpdateLLMProvider(ctx, llms.ProviderReplacement{ID: "second", Models: declaredModels()}); err != nil {
		t.Fatalf("clear second provider models: %v", err)
	}
	assertModelIdentityPresence(t, ctx, store, "shared", false)
}

// The default model is still referenced, so a connection write must not sweep it
// even when nothing binds it.
func TestIntegrationProviderWriteKeepsTheDefaultModelIdentity(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	key := "gateway-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "gateway", BaseURL: "https://gateway.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &key,
		Models: declaredModels(llms.ModelSpec{ID: "gpt-4o"}),
	}); err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := store.SetDefaultLLMModel(ctx, llms.ModelReference{ProviderID: "gateway", ModelID: "gpt-5-preview"}); err != nil {
		t.Fatalf("set default: %v", err)
	}

	otherKey := "other-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "other", BaseURL: "https://other.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &otherKey,
	}); err != nil {
		t.Fatalf("create other provider: %v", err)
	}
	if err := store.DeleteLLMProvider(ctx, "other"); err != nil {
		t.Fatalf("delete other provider: %v", err)
	}

	// gpt-5-preview is named by the default and gpt-4o is still bound, so both
	// identities must survive the sweep.
	assertModelIdentityPresence(t, ctx, store, "gpt-5-preview", true)
	assertModelIdentityPresence(t, ctx, store, "gpt-4o", true)
	if providerID, modelID, ok, err := store.DefaultLLMModelReference(ctx); err != nil || !ok || providerID != "gateway" || modelID != "gpt-5-preview" {
		t.Fatalf("default = %q/%q ok=%v err=%v, want it untouched", providerID, modelID, ok, err)
	}
}

// models.json declaring a default is a claim that a connection the file declares
// can serve that model. A reference to a connection the file does not control, or
// to one without a credential, was stored as the effective default while
// resolution ignored it: GetDefaultModel reported a default no run receives, and
// a run that named no model was routed by the connection fallback instead.
func TestIntegrationModelCatalogRejectsAnUnusableDefault(t *testing.T) {
	clearLLMTestEnvironment(t)
	cases := []struct {
		name      string
		reference string
		apiKey    string
		wantErr   string
	}{
		{name: "declared and enabled", reference: "gateway/gpt-4o", apiKey: "gateway-key"},
		{name: "connection the file does not declare", reference: "ghost/gpt-4o", apiKey: "gateway-key", wantErr: "does not declare"},
		{name: "connection without a credential and without an explicit enabled", reference: "gateway/gpt-4o", wantErr: `is disabled: declare an apiKey, or "enabled": true`},
		{name: "not a provider/model reference", reference: "gateway", apiKey: "gateway-key", wantErr: "not a <provider>/<model> reference"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newProjectionTestStore(t)
			baseURL := "https://gateway.example/v1"
			definition := llms.CatalogProvider{BaseURL: &baseURL, Models: []llms.CatalogModel{{ID: "gpt-4o"}}}
			if tc.apiKey != "" {
				apiKey := tc.apiKey
				definition.APIKey = &apiKey
			}
			err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{
				Default:   tc.reference,
				Providers: map[string]llms.CatalogProvider{"gateway": definition},
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ApplyModelCatalog error = %v, want success", err)
				}
				providerID, modelID, ok, refErr := store.DefaultLLMModelReference(ctx)
				if refErr != nil || !ok || providerID != "gateway" || modelID != "gpt-4o" {
					t.Fatalf("stored default = %q/%q ok=%v err=%v", providerID, modelID, ok, refErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ApplyModelCatalog error = %v, want it to mention %q", err, tc.wantErr)
			}
			if _, _, ok, refErr := store.DefaultLLMModelReference(ctx); refErr != nil || ok {
				t.Fatalf("default after a rejected reference ok=%v err=%v, want none", ok, refErr)
			}
		})
	}
}

// A connection another source owns is rejected as well: models.json does not
// control it, so a default written in the file could never be served by it.
func TestIntegrationModelCatalogRejectsADefaultOwnedByAnotherScope(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	managedKey := "managed-key"
	if _, err := store.CreateLLMProvider(ctx, llms.ProviderReplacement{
		ID: "managed", BaseURL: "https://managed.example/v1", Protocol: llms.APIProtocolResponses, APIKey: &managedKey,
	}); err != nil {
		t.Fatalf("create api provider: %v", err)
	}
	baseURL := "https://gateway.example/v1"
	fileKey := "gateway-key"
	err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{
		Default: "managed/gpt-4o",
		Providers: map[string]llms.CatalogProvider{
			"gateway": {BaseURL: &baseURL, APIKey: &fileKey, Models: []llms.CatalogModel{{ID: "gpt-4o"}}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `which scope "api" owns`) {
		t.Fatalf("ApplyModelCatalog error = %v, want the api-owned connection rejected", err)
	}
}

func assertModelIdentityPresence(t *testing.T, ctx context.Context, store *ConfigStore, modelID string, want bool) {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(1) FROM llm_model WHERE id = ?`, modelID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if got := count > 0; got != want {
		t.Fatalf("model identity %q present = %v, want %v", modelID, got, want)
	}
}
