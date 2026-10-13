package llms

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadModelCatalogLoadsDeclaredProvidersAndResolvesSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), ModelsCatalogFilename)
	data := `{
  "default": "baizhi/deepseek-v4-flash",
  "providers": {
    "baizhi": {
      "baseUrl": "https://gateway.example/api/openai",
      "protocol": "chat_completions",
      "apiKey": "${BAIZHI_API_KEY}",
      "models": [{"id":"deepseek-v4-flash","maxOutputTokens":99999}]
    },
    "openai": {
      "baseUrl":"https://api.openai.com/v1",
      "protocol":"responses",
      "apiKey":"$OPENAI_API_KEY"
    }
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"BAIZHI_API_KEY": "baizhi-secret", "OPENAI_API_KEY": "openai-secret"}
	catalog, err := LoadModelCatalog(path, func(name string) (string, bool) { value, ok := values[name]; return value, ok })
	if err != nil {
		t.Fatalf("LoadModelCatalog: %v", err)
	}
	if catalog.Default != "baizhi/deepseek-v4-flash" {
		t.Fatalf("default = %q", catalog.Default)
	}
	baizhi := catalog.Providers["baizhi"]
	if pointerStringForTest(baizhi.APIKey) != "baizhi-secret" || len(baizhi.Models) != 1 || baizhi.Models[0].MaxOutputTokens == nil || *baizhi.Models[0].MaxOutputTokens != 99999 {
		t.Fatalf("baizhi provider = %#v", baizhi)
	}
	openAI := catalog.Providers["openai"]
	if pointerStringForTest(openAI.BaseURL) != "https://api.openai.com/v1" || pointerStringForTest(openAI.APIKey) != "openai-secret" {
		t.Fatalf("openai provider = %#v", openAI)
	}
}

func TestLoadModelCatalogModelsAreOptionalAndMissingSecretFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), ModelsCatalogFilename)
	data := `{"default":"custom/literal-model","providers":{"custom":{"baseUrl":"https://example.test/v1","protocol":"responses","apiKey":"$MISSING"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadModelCatalog(path, func(string) (string, bool) { return "", false })
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("LoadModelCatalog error = %v", err)
	}

	data = `{"default":"custom/literal-model","providers":{"custom":{"baseUrl":"https://example.test/v1","protocol":"responses"}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadModelCatalog(path, nil)
	if err != nil {
		t.Fatalf("LoadModelCatalog without models: %v", err)
	}
	if len(catalog.Providers["custom"].Models) != 0 {
		t.Fatalf("custom models = %#v", catalog.Providers["custom"].Models)
	}
}

func TestLoadModelCatalogMissingFileReturnsEmptyCatalog(t *testing.T) {
	catalog, err := LoadModelCatalog(filepath.Join(t.TempDir(), ModelsCatalogFilename), nil)
	if err != nil {
		t.Fatalf("LoadModelCatalog: %v", err)
	}
	if catalog.Default != "" || len(catalog.Providers) != 0 {
		t.Fatalf("catalog = %#v, want empty catalog", catalog)
	}
}

func TestLoadModelCatalogRejectsIncompleteProvidersAndInvalidModels(t *testing.T) {
	for _, test := range []struct{ name, data, want string }{
		{name: "missing base URL", data: `{"providers":{"openai":{"protocol":"responses"}}}`, want: "requires baseUrl"},
		{name: "missing protocol", data: `{"providers":{"anthropic":{"baseUrl":"https://example.test"}}}`, want: "supported protocol"},
		{name: "unknown protocol", data: `{"providers":{"custom":{"baseUrl":"https://example.test","protocol":"unknown"}}}`, want: "supported protocol"},
		{name: "noncanonical messages protocol", data: `{"providers":{"anthropic":{"baseUrl":"https://example.test","protocol":"messages"}}}`, want: "supported protocol"},
		{name: "noncanonical token field", data: `{"providers":{"custom":{"baseUrl":"https://example.test","protocol":"responses","models":[{"id":"model","max_output_tokens":1}]}}}`, want: "unknown field"},
		{name: "duplicate model", data: `{"providers":{"custom":{"baseUrl":"https://example.test","protocol":"responses","models":[{"id":"same"},{"id":"same"}]}}}`, want: "more than once"},
		{name: "anthropic model protocol on openai provider", data: `{"providers":{"gateway":{"baseUrl":"https://example.test/v1","protocol":"responses","models":[{"id":"model","protocol":"anthropic_messages"}]}}}`, want: `protocol "anthropic_messages" is incompatible with provider family "openai"`},
		{name: "openai model protocol on anthropic provider", data: `{"providers":{"gateway":{"baseUrl":"https://example.test/v1","protocol":"anthropic_messages","models":[{"id":"model","protocol":"chat_completions"}]}}}`, want: `protocol "chat_completions" is incompatible with provider family "anthropic"`},
		{name: "non-positive max output tokens", data: `{"providers":{"custom":{"baseUrl":"https://example.test","protocol":"responses","models":[{"id":"model","maxOutputTokens":0}]}}}`, want: "must be positive"},
		{name: "max output tokens above the reportable range", data: `{"providers":{"custom":{"baseUrl":"https://example.test","protocol":"responses","models":[{"id":"model","maxOutputTokens":2147483648}]}}}`, want: "must not exceed 2147483647"},
		{name: "provider header name with a space", data: `{"providers":{"custom":{"baseUrl":"https://example.test","protocol":"responses","headers":{"X Tier":"batch"}}}}`, want: "is not a valid HTTP field name"},
		{name: "model header value with a newline", data: `{"providers":{"custom":{"baseUrl":"https://example.test","protocol":"responses","models":[{"id":"model","headers":{"x-tier":"batch\nx-key: injected"}}]}}}`, want: "has an invalid value"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ModelsCatalogFilename)
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadModelCatalog(path, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadModelCatalog error = %v", err)
			}
		})
	}
}

func TestLoadModelCatalogAcceptsCompatibleModelProtocolOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), ModelsCatalogFilename)
	data := `{"providers":{"openai":{"baseUrl":"https://openai.example/v1","protocol":"responses","models":[{"id":"chat-model","protocol":"chat_completions"}]},"anthropic":{"baseUrl":"https://anthropic.example/v1","protocol":"anthropic_messages","models":[{"id":"messages-model","protocol":"anthropic_messages"}]}}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadModelCatalog(path, nil); err != nil {
		t.Fatalf("LoadModelCatalog: %v", err)
	}
}

func pointerStringForTest(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// A connection that needs no credential must be able to say so, otherwise
// models.json cannot declare a local endpoint at all: a declaration without an
// apiKey used to be persisted disabled, so the file could neither serve a model
// from it nor name it as the default.
func TestLoadModelCatalogReadsTheDeclaredEnabledFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), ModelsCatalogFilename)
	data := `{
  "providers": {
    "local": {
      "baseUrl": "http://127.0.0.1:11434/v1",
      "protocol": "chat_completions",
      "enabled": true,
      "models": [{"id":"local-model"}]
    },
    "batch": {
      "baseUrl": "https://gateway.example/v1",
      "protocol": "responses",
      "apiKey": "secret",
      "enabled": false,
      "models": [{"id":"batch-model"}]
    }
  }
}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	catalog, err := LoadModelCatalog(path, nil)
	if err != nil {
		t.Fatalf("LoadModelCatalog: %v", err)
	}
	local := catalog.Providers["local"]
	if local.Enabled == nil || !*local.Enabled {
		t.Fatalf("local enabled = %v, want an explicit true", local.Enabled)
	}
	batch := catalog.Providers["batch"]
	if batch.Enabled == nil || *batch.Enabled {
		t.Fatalf("batch enabled = %v, want an explicit false", batch.Enabled)
	}
}
