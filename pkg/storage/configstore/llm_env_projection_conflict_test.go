package configstore

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/chaitin/agent-compose/pkg/llms"
)

// The daemon environment is a bootstrap fallback, so it must not take over an id
// models.json already declared. Before the ownership guard the projection
// rewrote the catalog row's scope to env_default and replaced its connection
// details, silently discarding the operator's models.json connection.
func TestIntegrationEnvProjectionKeepsCatalogOwnedProvider(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	insertCatalogProviderModel(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, "catalog-model", "chat_completions", "")

	err := store.UpsertDefaultLLMConfig(ctx, envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("env-model"))
	conflict, ok := llms.AsDefaultConfigConflict(err)
	if !ok {
		t.Fatalf("UpsertDefaultLLMConfig error = %v, want a default config conflict", err)
	}
	if conflict.Kind != "provider" || conflict.ID != llms.ProviderIDDefaultOpenAI || conflict.Scope != llms.ProviderScopeCatalog {
		t.Fatalf("conflict = %#v", conflict)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeCatalog, "https://gateway.example/v1")
	if count := countProviderModelBindings(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI); count != 1 {
		t.Fatalf("bindings = %d, want the catalog binding only", count)
	}
}

// A same-id model is also an ownership boundary: an environment run must not
// flip the default flag or the scope of a model another source serves.
func TestIntegrationEnvProjectionKeepsCatalogOwnedModel(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	insertEnvScopedProvider(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI)
	insertServingCatalogConnection(t, ctx, store.DB(), "catalog-connection", "shared-model")

	err := store.UpsertDefaultLLMConfig(ctx, envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("shared-model"))
	conflict, ok := llms.AsDefaultConfigConflict(err)
	if !ok {
		t.Fatalf("UpsertDefaultLLMConfig error = %v, want a default config conflict", err)
	}
	if conflict.Kind != "model" || conflict.ID != "shared-model" || conflict.Scope != llms.ProviderScopeCatalog {
		t.Fatalf("conflict = %#v", conflict)
	}
	var scope string
	var defaultModel int
	if err := store.DB().QueryRowContext(ctx, `SELECT scope, default_model FROM llm_model WHERE id = ?`, "shared-model").Scan(&scope, &defaultModel); err != nil {
		t.Fatal(err)
	}
	if scope != llms.ProviderScopeCatalog || defaultModel != 0 {
		t.Fatalf("catalog model scope = %q default = %d, want it untouched", scope, defaultModel)
	}
}

// The projection still refreshes the rows it owns, so restarting the daemon with
// changed environment values keeps working.
func TestIntegrationEnvProjectionUpdatesItsOwnRows(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	provider := envProjectionProvider(llms.ProviderIDDefaultOpenAI)
	if err := store.UpsertDefaultLLMConfig(ctx, provider, envProjectionModel("env-model")); err != nil {
		t.Fatalf("first projection: %v", err)
	}
	provider.BaseURL = "https://rotated.example/v1"
	if err := store.UpsertDefaultLLMConfig(ctx, provider, envProjectionModel("env-model")); err != nil {
		t.Fatalf("second projection: %v", err)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeEnvDefault, "https://rotated.example/v1")
}

// A model identity the file no longer declares is released with the rest of the
// dropped connection, so the environment bootstrap may take the id over — which is
// what declaring the same model in the environment means. Reading such a row as an
// owner made a model an operator had removed from models.json block the whole
// environment projection forever, leaving a daemon that silently lost its LLM_*
// configuration with only a startup warning to explain it.
func TestIntegrationEnvProjectionTakesOverAReleasedModelIdentity(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	if err := store.ApplyModelCatalog(ctx, catalogWithCredential("gateway", "shared-model")); err != nil {
		t.Fatalf("declare the model: %v", err)
	}
	if err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{}); err != nil {
		t.Fatalf("release the model: %v", err)
	}

	err := store.UpsertDefaultLLMConfig(ctx, envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("shared-model"))
	if err != nil {
		t.Fatalf("UpsertDefaultLLMConfig error = %v, want the released model id taken over", err)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeEnvDefault, "https://env.example/v1")
	var scope string
	var enabled, defaultModel int
	if err := store.DB().QueryRowContext(ctx, `SELECT scope, enabled, default_model FROM llm_model WHERE id = ?`, "shared-model").Scan(&scope, &enabled, &defaultModel); err != nil {
		t.Fatal(err)
	}
	if scope != llms.ProviderScopeEnvDefault || enabled == 0 || defaultModel == 0 {
		t.Fatalf("model scope = %q enabled = %d default = %d, want the environment row to become the default", scope, enabled, defaultModel)
	}
}

// A projection that yields releases the connection it materialized. Left enabled,
// that row would stay a peer of the declaring source for the conflicting model,
// and connection selection — which deliberately does not rank configuration
// sources — would hand a run to either of them at random. The released row is
// whichever scope this projection owns: the env_default it writes today, or the
// legacy system default older versions wrote.
func TestIntegrationEnvProjectionReleasesItsConnectionWhenItYields(t *testing.T) {
	clearLLMTestEnvironment(t)
	for _, scope := range []string{llms.ProviderScopeEnvDefault, llms.ProviderScopeSystem} {
		t.Run(scope, func(t *testing.T) {
			ctx := context.Background()
			store := newProjectionTestStore(t)

			projected := envProjectionProvider(llms.ProviderIDDefaultOpenAI)
			projected.Scope = scope
			if err := store.UpsertDefaultLLMConfig(ctx, projected, envProjectionModel("shared-model")); err != nil {
				t.Fatalf("first environment projection: %v", err)
			}
			if err := store.ApplyModelCatalog(ctx, catalogWithCredential("gateway", "shared-model")); err != nil {
				t.Fatalf("the file declares the same model: %v", err)
			}
			if err := store.UpsertDefaultLLMConfig(ctx, projected, envProjectionModel("shared-model")); err == nil {
				t.Fatal("the environment projection did not yield to the file that declares the model")
			}
			assertReleased(t, ctx, store.DB(), "llm_provider", llms.ProviderIDDefaultOpenAI)

			loaded, err := llms.LoadCatalog(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.DefaultModel(); got != "shared-model" {
				t.Fatalf("DefaultModel() = %q, want the file's connection to serve the model", got)
			}
			for i := 0; i < 8; i++ {
				target, err := loaded.Resolve("", "shared-model", llms.DefaultProtocolPreference())
				if err != nil {
					t.Fatal(err)
				}
				if target.Provider.ID != "gateway" {
					t.Fatalf("resolved connection = %q, want only the file's connection to serve the model", target.Provider.ID)
				}
			}

			// Releasing is not permanent: once the file stops declaring the model, the
			// next projection materializes the environment connection again.
			if err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{}); err != nil {
				t.Fatalf("drop the declaration: %v", err)
			}
			if err := store.UpsertDefaultLLMConfig(ctx, envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("shared-model")); err != nil {
				t.Fatalf("recover the released connection: %v", err)
			}
			assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeEnvDefault, "https://env.example/v1")
			recovered, err := llms.LoadCatalog(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			if got := recovered.DefaultModel(); got != "shared-model" {
				t.Fatalf("DefaultModel() = %q after recovery, want the environment model", got)
			}
		})
	}
}

// A models.json Provider keeps its id while the file declares it, even though a
// declaration without an apiKey is persisted disabled. Taking that id over
// rewrote the row's scope, and the file projection can only rewrite a provider
// row it still owns (`WHERE scope = 'catalog'`): the next startup's projection
// matched no row, failed with a non-catalog conflict, and loadLLMConfig failed
// with it, so the daemon started without its cleanup, scheduler, and deletion
// recovery managers — on every startup, with no way to recover.
// A provider id the file declares without a credential is persisted disabled, so it
// serves nothing and must not keep the bootstrap from serving that id: yielding let
// a deployment end up with every connection disabled, which is the same outcome the
// model axis already prevents. The file keeps the declaration either way — its
// projection accepts the scope the bootstrap writes — so it takes the id back on the
// next startup and keeps it as soon as it can serve.
func TestIntegrationEnvProjectionTakesOverADisabledDeclaredProvider(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	catalog := credentiallessCatalog(llms.ProviderIDDefaultOpenAI, "shared-model")
	envProvider, envModel := envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("env-model")
	if err := store.ApplyModelCatalog(ctx, catalog); err != nil {
		t.Fatalf("first file projection: %v", err)
	}
	if err := store.UpsertDefaultLLMConfig(ctx, envProvider, envModel); err != nil {
		t.Fatalf("UpsertDefaultLLMConfig error = %v, want the bootstrap to serve the id the file cannot", err)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeEnvDefault, envProvider.BaseURL)
	loaded, err := llms.LoadCatalog(ctx, store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if _, err := loaded.Resolve("", "env-model", nil); err != nil {
		t.Fatalf("Resolve(env-model) error = %v, want the bootstrap's connection to serve", err)
	}

	// The file still rewrites the row it declares, so the next startup projects it
	// from the file and the bootstrap serves the id again.
	if err := store.ApplyModelCatalog(ctx, catalog); err != nil {
		t.Fatalf("second file projection: %v", err)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeCatalog, "https://catalog.example/v1")
	if err := store.UpsertDefaultLLMConfig(ctx, envProvider, envModel); err != nil {
		t.Fatalf("UpsertDefaultLLMConfig after the file rewrote its row error = %v, want the bootstrap to serve again", err)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeEnvDefault, envProvider.BaseURL)

	// Once the file can serve, the id is the file's and the bootstrap yields.
	if err := store.ApplyModelCatalog(ctx, catalogWithCredential(llms.ProviderIDDefaultOpenAI, "shared-model")); err != nil {
		t.Fatalf("file projection with a credential: %v", err)
	}
	err = store.UpsertDefaultLLMConfig(ctx, envProvider, envModel)
	conflict, ok := llms.AsDefaultConfigConflict(err)
	if !ok {
		t.Fatalf("UpsertDefaultLLMConfig error = %v, want a default config conflict", err)
	}
	if conflict.Kind != "provider" || conflict.ID != llms.ProviderIDDefaultOpenAI || conflict.Scope != llms.ProviderScopeCatalog {
		t.Fatalf("conflict = %#v", conflict)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeCatalog, "https://catalog.example/v1")
	var enabled int
	if err := store.DB().QueryRowContext(ctx, `SELECT enabled FROM llm_provider WHERE id = ?`, llms.ProviderIDDefaultOpenAI).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled == 0 {
		t.Fatal("the connection the file now serves was disabled by the bootstrap yielding")
	}
}

// credentiallessCatalog is a models.json catalog whose only Provider declares no
// apiKey, so the file projection persists it disabled while the file keeps
// declaring its id.
func credentiallessCatalog(providerID, modelID string) llms.ModelCatalog {
	baseURL, protocol := "https://catalog.example/v1", llms.APIProtocolResponses
	return llms.ModelCatalog{
		Providers: map[string]llms.CatalogProvider{
			providerID: {
				BaseURL:  &baseURL,
				Protocol: &protocol,
				Models:   []llms.CatalogModel{{ID: modelID}},
			},
		},
	}
}

// A connection the file stops declaring must not keep owning its id. Only the
// file projection rewrites a catalog row and the LLM service refuses to manage
// one, so a leftover row would make every later startup's environment projection
// yield, silently dropping the LLM_* bootstrap with no way to recover.
func TestIntegrationEnvProjectionTakesOverAConnectionTheFileDropped(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	if err := store.ApplyModelCatalog(ctx, catalogWithCredential(llms.ProviderIDDefaultOpenAI, "shared-model")); err != nil {
		t.Fatalf("declare the connection: %v", err)
	}
	if err := store.UpsertDefaultLLMConfig(ctx, envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("env-model")); err == nil {
		t.Fatal("the environment took over a connection models.json still declares")
	}

	// The operator removes the connection from models.json, or removes the file.
	if err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{}); err != nil {
		t.Fatalf("drop the connection: %v", err)
	}
	if err := store.UpsertDefaultLLMConfig(ctx, envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("env-model")); err != nil {
		t.Fatalf("UpsertDefaultLLMConfig error = %v, want the dropped id taken over", err)
	}
	assertProviderOwnership(t, ctx, store.DB(), llms.ProviderIDDefaultOpenAI, llms.ProviderScopeEnvDefault, "https://env.example/v1")
	// The file projection that runs first on later startups must still succeed.
	if err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{}); err != nil {
		t.Fatalf("file projection after the environment took the id over: %v", err)
	}
}

// catalogWithCredential is credentiallessCatalog with a usable credential, so the
// file projection declares an enabled connection.
func catalogWithCredential(providerID, modelID string) llms.ModelCatalog {
	catalog := credentiallessCatalog(providerID, modelID)
	key := "catalog-key"
	provider := catalog.Providers[providerID]
	provider.APIKey = &key
	catalog.Providers[providerID] = provider
	return catalog
}

func newProjectionTestStore(t *testing.T) *ConfigStore {
	t.Helper()
	store := FromDB(newMemoryDB(t))
	if err := store.InitSchema(context.Background()); err != nil {
		t.Fatalf("InitSchema: %v", err)
	}
	return store
}

func envProjectionProvider(id string) llms.Provider {
	return llms.Provider{
		ID:             id,
		Name:           "openai",
		ProviderType:   llms.ProviderFamilyOpenAI,
		DefaultWireAPI: llms.APIProtocolResponses,
		BaseURL:        "https://env.example/v1",
		APIKey:         "env-key",
		AuthHeader:     "Authorization",
		AuthScheme:     "Bearer",
		HeadersJSON:    "{}",
		Weight:         10,
		Enabled:        true,
		Scope:          llms.ProviderScopeEnvDefault,
	}
}

func envProjectionModel(id string) llms.Model {
	return llms.Model{ID: id, Name: id, DefaultModel: true, Enabled: true, Scope: llms.ProviderScopeEnvDefault}
}

func insertEnvScopedProvider(t *testing.T, ctx context.Context, db *sql.DB, providerID string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO llm_provider(
		id, name, provider_type, default_wire_api, base_url, api_key, auth_header, auth_scheme, headers_json, weight, enabled, scope, created_at, updated_at)
		VALUES(?, ?, 'openai', 'responses', 'https://env.example/v1', 'env-key', 'Authorization', 'Bearer', '{}', 10, 1, 'env_default', 0, 0)`,
		providerID, providerID); err != nil {
		t.Fatalf("insert env provider: %v", err)
	}
}

func insertCatalogOwnedModel(t *testing.T, ctx context.Context, db *sql.DB, modelID string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO llm_model(id, name, description, default_model, enabled, scope, created_at, updated_at)
		VALUES(?, ?, '', 0, 1, 'catalog', 0, 0)`, modelID, modelID); err != nil {
		t.Fatalf("insert catalog model: %v", err)
	}
}

func assertProviderOwnership(t *testing.T, ctx context.Context, db *sql.DB, providerID, wantScope, wantBaseURL string) {
	t.Helper()
	var scope, baseURL string
	if err := db.QueryRowContext(ctx, `SELECT scope, base_url FROM llm_provider WHERE id = ?`, providerID).Scan(&scope, &baseURL); err != nil {
		t.Fatal(err)
	}
	if scope != wantScope || baseURL != wantBaseURL {
		t.Fatalf("provider %q scope = %q base_url = %q, want %q/%q", providerID, scope, baseURL, wantScope, wantBaseURL)
	}
}

func countProviderModelBindings(t *testing.T, ctx context.Context, db *sql.DB, providerID string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(1) FROM llm_provider_model WHERE provider_id = ?`, providerID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Guard the sentinel contract the startup boundary relies on for errors.Is.
func TestDefaultConfigConflictIsClassifiable(t *testing.T) {
	wrapped := errors.Join(errors.New("outer"), &llms.DefaultConfigConflict{Kind: "provider", ID: "default", Scope: "catalog"})
	if !errors.Is(wrapped, llms.ErrDefaultConfigConflict) {
		t.Fatal("wrapped conflict does not match the sentinel")
	}
	if _, ok := llms.AsDefaultConfigConflict(wrapped); !ok {
		t.Fatal("AsDefaultConfigConflict did not extract the conflict")
	}
}

// A models.json declaration that cannot serve must not suppress the environment
// bootstrap. A connection without a credential is persisted disabled, so its
// model identity answers no request at all; before this guard the environment
// yielded to that identity, released its own connection, and left the deployment
// with no enabled connection and no daemon-managed credential.
func TestIntegrationEnvProjectionKeepsAModelNoEnabledConnectionServes(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	baseURL, protocol := "https://catalog.example/v1", llms.APIProtocolResponses
	if err := store.ApplyModelCatalog(ctx, llms.ModelCatalog{Providers: map[string]llms.CatalogProvider{
		"local": {BaseURL: &baseURL, Protocol: &protocol, Models: []llms.CatalogModel{{ID: "gpt-4o"}}},
	}}); err != nil {
		t.Fatalf("ApplyModelCatalog: %v", err)
	}

	if err := store.UpsertDefaultLLMConfig(ctx, envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("gpt-4o")); err != nil {
		t.Fatalf("UpsertDefaultLLMConfig error = %v, want the bootstrap to materialize its connection", err)
	}
	providers, err := store.ListEnabledLLMProviders(ctx)
	if err != nil {
		t.Fatalf("ListEnabledLLMProviders: %v", err)
	}
	serving := false
	for _, provider := range providers {
		if provider.ID == llms.ProviderIDDefaultOpenAI {
			serving = true
		}
	}
	if !serving {
		t.Fatalf("enabled connections = %#v, want the environment connection", providers)
	}

	loaded, err := llms.LoadCatalog(ctx, store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	selected, err := loaded.SelectModel("")
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}
	if selected != "gpt-4o" {
		t.Fatalf("SelectModel = %q, want gpt-4o", selected)
	}
	target, err := loaded.Resolve("", selected, nil)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", selected, err)
	}
	if target.Provider.ID != llms.ProviderIDDefaultOpenAI {
		t.Fatalf("resolved by %q, want the environment connection %q", target.Provider.ID, llms.ProviderIDDefaultOpenAI)
	}
}

// The file projection reclaims a model identity's scope on every startup, while
// the connection the environment materialized stays enabled. Reading that
// connection as the identity's owner made the bootstrap treat its own row as
// another source's and release it on the second start, so the deployment lost the
// only connection that could serve the model.
func TestIntegrationEnvProjectionKeepsItsOwnConnectionAcrossRestarts(t *testing.T) {
	clearLLMTestEnvironment(t)
	ctx := context.Background()
	store := newProjectionTestStore(t)

	baseURL, protocol := "https://catalog.example/v1", llms.APIProtocolResponses
	file := llms.ModelCatalog{Providers: map[string]llms.CatalogProvider{
		"local": {BaseURL: &baseURL, Protocol: &protocol, Models: []llms.CatalogModel{{ID: "gpt-4o"}}},
	}}
	provider, model := envProjectionProvider(llms.ProviderIDDefaultOpenAI), envProjectionModel("gpt-4o")
	for start := 1; start <= 2; start++ {
		if err := store.ApplyModelCatalog(ctx, file); err != nil {
			t.Fatalf("start %d: ApplyModelCatalog: %v", start, err)
		}
		if err := store.UpsertDefaultLLMConfig(ctx, provider, model); err != nil {
			t.Fatalf("start %d: UpsertDefaultLLMConfig error = %v, want the bootstrap to keep its own connection", start, err)
		}
		providers, err := store.ListEnabledLLMProviders(ctx)
		if err != nil {
			t.Fatalf("start %d: ListEnabledLLMProviders: %v", start, err)
		}
		serving := false
		for _, candidate := range providers {
			if candidate.ID == llms.ProviderIDDefaultOpenAI {
				serving = true
			}
		}
		if !serving {
			t.Fatalf("start %d: enabled connections = %#v, want the environment connection", start, providers)
		}
	}

	loaded, err := llms.LoadCatalog(ctx, store)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	selected, err := loaded.SelectModel("")
	if err != nil {
		t.Fatalf("SelectModel: %v", err)
	}
	target, err := loaded.Resolve("", selected, nil)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", selected, err)
	}
	if target.Provider.ID != llms.ProviderIDDefaultOpenAI {
		t.Fatalf("resolved by %q, want the environment connection %q", target.Provider.ID, llms.ProviderIDDefaultOpenAI)
	}
}

// insertServingCatalogConnection declares a catalog identity that an enabled
// connection actually binds, which is what makes it an owner the environment
// bootstrap must not take an id from.
func insertServingCatalogConnection(t *testing.T, ctx context.Context, db *sql.DB, providerID, modelID string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `INSERT INTO llm_provider(
		id, name, provider_type, default_wire_api, base_url, api_key, auth_header, auth_scheme, headers_json, weight, enabled, scope, created_at, updated_at)
		VALUES(?, ?, 'openai', 'chat_completions', 'https://catalog.example/v1', 'catalog-key', 'Authorization', 'Bearer', '{}', 10, 1, 'catalog', 0, 0)`,
		providerID, providerID); err != nil {
		t.Fatalf("insert catalog provider: %v", err)
	}
	insertCatalogOwnedModel(t, ctx, db, modelID)
	if _, err := db.ExecContext(ctx, `INSERT INTO llm_provider_model(
		provider_id, model_id, wire_api, weight, base_url, headers_json, max_output_tokens, display_name)
		VALUES(?, ?, 'chat_completions', 10, '', '{}', 0, '')`, providerID, modelID); err != nil {
		t.Fatalf("insert catalog binding: %v", err)
	}
}
