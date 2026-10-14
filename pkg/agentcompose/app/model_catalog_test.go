package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/llms"
)

type modelCatalogStoreFake struct {
	catalog llms.ModelCatalog
	err     error
	// defaultProvider/defaultModel/hasDefault is the stored default, and
	// catalogDefault is what applying the catalog leaves stored, so a test can
	// observe whether projecting the file replaced one.
	defaultProvider string
	defaultModel    string
	hasDefault      bool
	catalogDefault  *llms.ModelReference
}

func (s *modelCatalogStoreFake) ApplyModelCatalog(_ context.Context, catalog llms.ModelCatalog) error {
	s.catalog = catalog
	if s.catalogDefault != nil {
		s.defaultProvider, s.defaultModel, s.hasDefault = s.catalogDefault.ProviderID, s.catalogDefault.ModelID, true
	}
	return s.err
}

func (s *modelCatalogStoreFake) DefaultLLMModelReference(context.Context) (string, string, bool, error) {
	return s.defaultProvider, s.defaultModel, s.hasDefault, nil
}

func TestLoadModelCatalogUsesDataRootAndFailsUnresolvedSecrets(t *testing.T) {
	dataRoot := t.TempDir()
	t.Setenv("CATALOG_TEST_KEY", "resolved-key")
	data := `{"default":"custom/literal","providers":{"custom":{"baseUrl":"https://example.test/v1","protocol":"responses","apiKey":"$CATALOG_TEST_KEY"}}}`
	if err := os.WriteFile(filepath.Join(dataRoot, llms.ModelsCatalogFilename), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	store := &modelCatalogStoreFake{}
	if _, err := loadModelCatalog(context.Background(), &appconfig.Config{DataRoot: dataRoot}, store); err != nil {
		t.Fatal(err)
	}
	if store.catalog.Default == nil || *store.catalog.Default != "custom/literal" || *store.catalog.Providers["custom"].APIKey != "resolved-key" {
		t.Fatalf("catalog = %#v", store.catalog)
	}

	t.Setenv("CATALOG_TEST_KEY", "")
	if _, err := loadModelCatalog(context.Background(), &appconfig.Config{DataRoot: dataRoot}, store); err == nil || !strings.Contains(err.Error(), "CATALOG_TEST_KEY") {
		t.Fatalf("loadModelCatalog error = %v", err)
	}
}

func TestIntegrationLoadModelCatalogRejectsCrossFamilyModelProtocol(t *testing.T) {
	dataRoot := t.TempDir()
	data := `{"providers":{"gateway":{"baseUrl":"https://gateway.example/v1","protocol":"responses","models":[{"id":"model","protocol":"anthropic_messages"}]}}}`
	if err := os.WriteFile(filepath.Join(dataRoot, llms.ModelsCatalogFilename), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	store := &modelCatalogStoreFake{}
	_, err := loadModelCatalog(context.Background(), &appconfig.Config{DataRoot: dataRoot}, store)
	if err == nil || !strings.Contains(err.Error(), `incompatible with provider family "openai"`) {
		t.Fatalf("loadModelCatalog error = %v", err)
	}
	if store.catalog.Default != nil || len(store.catalog.Providers) != 0 {
		t.Fatalf("store received catalog after validation failure: %#v", store.catalog)
	}
}
