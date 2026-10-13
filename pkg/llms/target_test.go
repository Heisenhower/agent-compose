package llms

import (
	"testing"
)

func TestNewResolvedTargetDropsForbiddenPerModelHeaders(t *testing.T) {
	config := ProviderModelConfig{
		HeadersJSON: `{"Authorization":"Bearer stolen","X-Model-Header":"allowed"}`,
	}
	provider := Provider{
		ID:             "provider-1",
		BaseURL:        "https://provider.test",
		DefaultWireAPI: APIProtocolChatCompletions,
		AuthHeader:     "Authorization",
		AuthScheme:     "Bearer",
		APIKey:         "provider-secret",
	}
	model := Model{ID: "model-1", Name: "model-1"}

	target, err := NewResolvedTarget(provider, model, config)
	if err != nil {
		t.Fatalf("NewResolvedTarget returned error: %v", err)
	}
	if got := target.Headers.Get("Authorization"); got != "Bearer provider-secret" {
		t.Fatalf("Authorization header = %q, want provider credential to survive unclobbered", got)
	}
	if got := target.Headers.Get("X-Model-Header"); got != "allowed" {
		t.Fatalf("X-Model-Header = %q, want per-model header to pass through", got)
	}
}

// ForbiddenProviderHeader lower-cases and trims its "name" argument before
// comparing, so passing the raw (untrimmed) per-model key into it and only
// trimming for the later headers.Set call cannot disagree: trimming is
// idempotent, so canonical(rawKey) == canonical(trim(rawKey)) always holds.
// This test pins that guarantee against case and whitespace variants of the
// auth header name.
func TestNewResolvedTargetDropsForbiddenPerModelHeaderCaseAndWhitespaceVariants(t *testing.T) {
	for _, key := range []string{"authorization", " Authorization", "AUTHORIZATION", " authorization "} {
		t.Run(key, func(t *testing.T) {
			config := ProviderModelConfig{HeadersJSON: `{"` + key + `":"Bearer stolen"}`}
			provider := Provider{
				ID:             "provider-1",
				BaseURL:        "https://provider.test",
				DefaultWireAPI: APIProtocolChatCompletions,
				AuthHeader:     "Authorization",
				AuthScheme:     "Bearer",
				APIKey:         "provider-secret",
			}
			model := Model{ID: "model-1", Name: "model-1"}

			target, err := NewResolvedTarget(provider, model, config)
			if err != nil {
				t.Fatalf("NewResolvedTarget returned error: %v", err)
			}
			if got := target.Headers.Get("Authorization"); got != "Bearer provider-secret" {
				t.Fatalf("Authorization header = %q for per-model key %q, want provider credential to survive unclobbered", got, key)
			}
		})
	}
}

// A declared model that overrides the connection's base url is served on its own
// endpoint, so every diagnostic that names the connection must resolve that
// model's endpoint first: a probe that used the connection endpoint reported a
// verdict about an endpoint no request for the model ever reaches.
func TestProviderForModelResolvesTheDeclaredModelEndpoint(t *testing.T) {
	provider := Provider{ID: "azure", BaseURL: "https://connection.example/openai"}
	bindings := []ProviderModelBinding{
		{ProviderID: "azure", ModelID: "deployed", Config: ProviderModelConfig{BaseURL: "https://deployment.example/openai"}},
		{ProviderID: "azure", ModelID: "plain"},
		{ProviderID: "other", ModelID: "shared", Config: ProviderModelConfig{BaseURL: "https://other.example/openai"}},
	}
	cases := []struct {
		name  string
		model string
		want  string
	}{
		{name: "declared with an override", model: "deployed", want: "https://deployment.example/openai"},
		{name: "declared without an override", model: "plain", want: provider.BaseURL},
		{name: "another connection's model", model: "shared", want: provider.BaseURL},
		{name: "not declared", model: "missing", want: provider.BaseURL},
		{name: "no model named", model: "", want: provider.BaseURL},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ProviderForModel(provider, bindings, tc.model).BaseURL; got != tc.want {
				t.Fatalf("ProviderForModel(%q).BaseURL = %q, want %q", tc.model, got, tc.want)
			}
			if provider.BaseURL != "https://connection.example/openai" {
				t.Fatalf("the caller's connection was mutated: %q", provider.BaseURL)
			}
		})
	}
}
