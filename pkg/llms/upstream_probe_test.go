package llms

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestUpstreamProberReportsEverySupportedProtocol(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string][]byte{}
	record := func(r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies[r.URL.Path] = data
		mu.Unlock()
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			writeProbeJSON(w, `{"object":"list","data":[{"id":"z-model"},{"id":"a-model"},{"id":"z-model"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
			record(r)
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"}}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			record(r)
			writeProbeJSON(w, `{"object":"response","output":[{"type":"message"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := probeTestProvider(server.URL + "/v1")
	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{Provider: provider, Model: "probe-model"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if want := []string{"a-model", "z-model"}; !reflect.DeepEqual(result.Models, want) {
		t.Fatalf("models = %v, want %v", result.Models, want)
	}
	if !result.Proven() {
		t.Fatal("Proven() = false for a reachable endpoint")
	}
	if !result.Supports(ProtocolChatCompletions) || !result.Supports(ProtocolResponses) {
		t.Fatalf("protocols = %#v, want both OpenAI protocols supported on one endpoint", result.Protocols)
	}
	if got := result.SupportedProtocols(); !reflect.DeepEqual(got, []Protocol{ProtocolChatCompletions, ProtocolResponses}) {
		t.Fatalf("SupportedProtocols() = %v", got)
	}

	assertProbeBody(t, bodies["/v1/chat/completions"], "messages", 1)
	assertProbeBody(t, bodies["/v1/responses"], "input", 1)
}

func TestUpstreamProberDistinguishesUnsupportedProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"data":[{"id":"chat-only"}]}`)
		case "/v1/chat/completions":
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := probeTestProvider(server.URL + "/v1")
	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{Provider: provider, Model: "chat-only"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Supports(ProtocolResponses) {
		t.Fatal("responses reported as supported against a chat-only endpoint")
	}
	if !result.Supports(ProtocolChatCompletions) {
		t.Fatal("chat_completions not reported as supported")
	}
	declared, supported, mismatch := DeclaredProtocolUnsupported(provider, result)
	if !mismatch || declared != ProtocolResponses {
		t.Fatalf("DeclaredProtocolUnsupported = %q/%v/%v, want a responses mismatch", declared, supported, mismatch)
	}
	if !reflect.DeepEqual(supported, []Protocol{ProtocolChatCompletions}) {
		t.Fatalf("supported = %v, want chat_completions", supported)
	}
}

// Only the declared protocol's own verdict is evidence about it. A sibling
// protocol succeeding, an inconclusive answer, or a protocol the probe never
// reached must not be reported as "the endpoint does not support the declared
// protocol".
func TestDeclaredProtocolUnsupportedRequiresTheDeclaredProtocolsVerdict(t *testing.T) {
	provider := probeTestProvider("https://gateway.example/v1")
	cases := []struct {
		name      string
		protocols []ProtocolProbe
		want      bool
	}{
		{
			name: "declared protocol proven absent",
			protocols: []ProtocolProbe{
				{Protocol: ProtocolChatCompletions, Outcome: ProbeSupported},
				{Protocol: ProtocolResponses, Outcome: ProbeUnsupported},
			},
			want: true,
		},
		{
			name: "declared protocol inconclusive while a sibling succeeded",
			protocols: []ProtocolProbe{
				{Protocol: ProtocolChatCompletions, Outcome: ProbeSupported},
				{Protocol: ProtocolResponses, Outcome: ProbeInconclusive},
			},
			want: false,
		},
		{
			name: "declared protocol not probed",
			protocols: []ProtocolProbe{
				{Protocol: ProtocolChatCompletions, Outcome: ProbeSupported},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			declared, supported, mismatch := DeclaredProtocolUnsupported(provider, UpstreamProbeResult{Protocols: tc.protocols})
			if declared != ProtocolResponses {
				t.Fatalf("declared = %q, want responses", declared)
			}
			if mismatch != tc.want {
				t.Fatalf("mismatch = %v, want %v", mismatch, tc.want)
			}
			if !reflect.DeepEqual(supported, []Protocol{ProtocolChatCompletions}) {
				t.Fatalf("supported = %v, want chat_completions", supported)
			}
		})
	}
}

// A 500 on the declared protocol is classified inconclusive, so an endpoint that
// answers its sibling protocol must not be reported as not supporting the
// declared one: the probe never proved that.
func TestUpstreamProberDoesNotBlameAnInconclusiveDeclaredProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"data":[{"id":"m"}]}`)
		case "/v1/chat/completions":
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	provider := probeTestProvider(server.URL + "/v1")
	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{Provider: provider, Model: "m"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if outcome, probed := result.Outcome(ProtocolResponses); !probed || outcome != ProbeInconclusive {
		t.Fatalf("responses verdict = %q probed=%v, want %q", outcome, probed, ProbeInconclusive)
	}
	if declared, supported, mismatch := DeclaredProtocolUnsupported(provider, result); mismatch {
		t.Fatalf("mismatch claimed for an inconclusive declared protocol: declared=%q supported=%v", declared, supported)
	}
}

func TestUpstreamProberKeepsProbingWhenModelListIsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{
		Provider: probeTestProvider(server.URL + "/v1"), Model: "chat-only",
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.ModelsDetail != "HTTP 404" {
		t.Fatalf("ModelsDetail = %q, want the inventory failure recorded", result.ModelsDetail)
	}
	if !result.Supports(ProtocolChatCompletions) {
		t.Fatal("a missing /v1/models endpoint must not stop the protocol probes")
	}
}

func TestUpstreamProberStaysInconclusiveWhenUnreachable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	provider := probeTestProvider(server.URL + "/v1")
	result, err := NewUpstreamProber(nil).Probe(context.Background(), UpstreamProbeRequest{Provider: provider, Model: "any"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Proven() {
		t.Fatalf("an unreachable endpoint proved a protocol verdict: %#v", result.Protocols)
	}
	if _, _, mismatch := DeclaredProtocolUnsupported(provider, result); mismatch {
		t.Fatal("an unreachable endpoint must not produce a declared-protocol mismatch")
	}
}

func TestUpstreamProberTreatsRejectedCredentialAsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		writeProbeJSON(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer server.Close()

	provider := probeTestProvider(server.URL + "/v1")
	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{Provider: provider, Model: "any"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, probe := range result.Protocols {
		if probe.Outcome != ProbeAuthFailed {
			t.Fatalf("outcome for %s = %q, want %q", probe.Protocol, probe.Outcome, ProbeAuthFailed)
		}
	}
	if _, _, mismatch := DeclaredProtocolUnsupported(provider, result); mismatch {
		t.Fatal("a credential rejection says nothing about protocol support")
	}
}

func TestUpstreamProberRequiresEndpoint(t *testing.T) {
	prober := NewUpstreamProber(nil)
	if _, err := prober.Probe(context.Background(), UpstreamProbeRequest{Provider: probeTestProvider(""), Model: "m"}); err == nil {
		t.Fatal("Probe accepted a connection without a base url")
	}
}

// A connection that declares no model and whose endpoint lists none cannot be
// probed at all. That is reported as inconclusive, never as "protocol absent".
func TestUpstreamProberReportsInconclusiveWhenNoModelIsAvailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			writeProbeJSON(w, `{"object":"list","data":[]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{Provider: probeTestProvider(server.URL + "/v1")})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Proven() {
		t.Fatalf("a connection with no model proved a verdict: %#v", result.Protocols)
	}
	for _, probe := range result.Protocols {
		if probe.Outcome != ProbeInconclusive {
			t.Fatalf("outcome for %s = %q, want %q", probe.Protocol, probe.Outcome, ProbeInconclusive)
		}
		// The endpoint was reached and really did return an empty list, so this
		// is the one case where blaming the model list is accurate.
		if !strings.Contains(probe.Detail, "listed none") {
			t.Fatalf("detail for %s = %q, want the empty model list reported", probe.Protocol, probe.Detail)
		}
	}
}

// An endpoint that was never reached must not be reported as having listed no
// models: the inventory request failed, so the daemon has no observation about
// the model list at all.
func TestUpstreamProberAttributesAnUnreachableModelList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()

	result, err := NewUpstreamProber(nil).Probe(context.Background(), UpstreamProbeRequest{Provider: probeTestProvider(server.URL + "/v1")})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.ModelsDetail == "" {
		t.Fatal("an unreachable endpoint recorded no model list detail")
	}
	for _, probe := range result.Protocols {
		if probe.Outcome != ProbeInconclusive {
			t.Fatalf("outcome for %s = %q, want %q", probe.Protocol, probe.Outcome, ProbeInconclusive)
		}
		if !strings.Contains(probe.Detail, "model list is unavailable") {
			t.Fatalf("detail for %s = %q, want the unavailable model list named", probe.Protocol, probe.Detail)
		}
	}
}

// The model list is the fallback model source: an endpoint that advertises a
// model can still be probed when the connection declares none.
func TestUpstreamProberUsesListedModelWhenNoneIsDeclared(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"object":"list","data":[{"id":"listed-model"}]}`)
		case "/v1/chat/completions":
			body, _ = io.ReadAll(r.Body)
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{Provider: probeTestProvider(server.URL + "/v1")})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if result.Model != "listed-model" {
		t.Fatalf("probe model = %q, want the listed model", result.Model)
	}
	if !result.Supports(ProtocolChatCompletions) {
		t.Fatal("the advertised model was not used for the protocol probe")
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode probe body: %v", err)
	}
	if payload["model"] != "listed-model" {
		t.Fatalf("probe named model %v, want the listed model", payload["model"])
	}
}

func TestClassifyProbeResponse(t *testing.T) {
	cases := []struct {
		name     string
		protocol Protocol
		status   int
		body     string
		want     ProbeOutcome
	}{
		{name: "chat success", protocol: ProtocolChatCompletions, status: 200, body: `{"object":"chat.completion","choices":[{"index":0}]}`, want: ProbeSupported},
		{name: "responses success", protocol: ProtocolResponses, status: 200, body: `{"object":"response","output":[]}`, want: ProbeSupported},
		{name: "messages success", protocol: ProtocolMessages, status: 200, body: `{"type":"message","content":[]}`, want: ProbeSupported},
		{name: "empty success", protocol: ProtocolResponses, status: 200, body: ``, want: ProbeInconclusive},
		{name: "html success", protocol: ProtocolResponses, status: 200, body: `<html>ok</html>`, want: ProbeInconclusive},
		{name: "wrong shape", protocol: ProtocolResponses, status: 200, body: `{"object":"chat.completion","choices":[]}`, want: ProbeInconclusive},
		{name: "not found", protocol: ProtocolResponses, status: 404, body: `not found`, want: ProbeUnsupported},
		{name: "not found naming a missing model", protocol: ProtocolResponses, status: 404, body: `{"error":{"message":"The model \u0060nope\u0060 does not exist","code":"model_not_found"}}`, want: ProbeInconclusive},
		{name: "not found for an absent route", protocol: ProtocolResponses, status: 404, body: `{"error":{"message":"route not found"}}`, want: ProbeUnsupported},
		{name: "method not allowed", protocol: ProtocolChatCompletions, status: 405, body: ``, want: ProbeUnsupported},
		{name: "not implemented", protocol: ProtocolMessages, status: 501, body: ``, want: ProbeUnsupported},
		{name: "unauthorized", protocol: ProtocolResponses, status: 401, body: `{"error":{"message":"bad key"}}`, want: ProbeAuthFailed},
		{name: "forbidden", protocol: ProtocolMessages, status: 403, body: ``, want: ProbeAuthFailed},
		{name: "semantic rejection", protocol: ProtocolResponses, status: 400, body: `{"error":{"message":"model not found"}}`, want: ProbeSupported},
		{name: "unprocessable", protocol: ProtocolChatCompletions, status: 422, body: `{"error":{"message":"max_tokens invalid"}}`, want: ProbeSupported},
		{name: "rate limited", protocol: ProtocolResponses, status: 429, body: ``, want: ProbeInconclusive},
		{name: "server error", protocol: ProtocolResponses, status: 500, body: ``, want: ProbeInconclusive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := ClassifyProbeResponse(tc.protocol, tc.status, []byte(tc.body))
			if probe.Outcome != tc.want {
				t.Fatalf("outcome = %q (detail %q), want %q", probe.Outcome, probe.Detail, tc.want)
			}
		})
	}
}

// The endpoint a probe reports is logged and returned with the verdict, so the
// credential a connection may carry in its URL must not survive into it.
func TestUpstreamProberReportsARedactedEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"object":"list","data":[{"id":"served-model"}]}`)
		case "/v1/responses":
			writeProbeJSON(w, `{"object":"response","output":[{"type":"message"}]}`)
		case "/v1/chat/completions":
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := probeTestProvider(strings.Replace(server.URL, "http://", "http://user:secret@", 1) + "?api-key=secret")
	result, err := NewUpstreamProber(server.Client()).Probe(context.Background(), UpstreamProbeRequest{Provider: provider, Model: "served-model"})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if strings.Contains(result.Endpoint, "secret") || strings.Contains(result.Endpoint, "api-key") {
		t.Fatalf("reported endpoint = %q, want the credential-bearing parts removed", result.Endpoint)
	}
}

// A failure detail quotes the URL the probe called, so the connection's own
// credential-bearing parts are removed from the excerpt as well.
func TestProbeDetailDropsTheEndpointCredentials(t *testing.T) {
	provider := probeTestProvider("https://user:secret@gateway.example/v1?api-key=secret")
	detail := probeDetail(provider, `Get "https://user:secret@gateway.example/v1/models?api-key=secret": dial tcp: timeout`)
	if strings.Contains(detail, "secret") {
		t.Fatalf("detail = %q, want the credential removed", detail)
	}
}

func TestRedactedProbeEndpointDropsCredentials(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, want string }{
		{name: "plain", endpoint: "https://gateway.example/v1", want: "https://gateway.example/v1"},
		{name: "userinfo", endpoint: "https://user:secret@gateway.example/v1", want: "https://gateway.example/v1"},
		{name: "query", endpoint: "https://gateway.example/v1?api-key=secret", want: "https://gateway.example/v1"},
		{name: "padded", endpoint: "  https://gateway.example/v1  ", want: "https://gateway.example/v1"},
		{name: "relative", endpoint: "gateway.example/v1?api-key=secret", want: "gateway.example/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactedProbeEndpoint(tc.endpoint); got != tc.want {
				t.Fatalf("redactedProbeEndpoint(%q) = %q, want %q", tc.endpoint, got, tc.want)
			}
		})
	}
}

func TestUpstreamProbeProtocolsStayInFamily(t *testing.T) {
	openai := probeTestProvider("https://gateway.example/v1")
	if got := UpstreamProbeProtocols(openai); !reflect.DeepEqual(got, []Protocol{ProtocolChatCompletions, ProtocolResponses}) {
		t.Fatalf("openai protocols = %v", got)
	}
	anthropic := openai
	anthropic.ProviderType = ProviderFamilyAnthropic
	anthropic.DefaultWireAPI = APIProtocolMessages
	if got := UpstreamProbeProtocols(anthropic); !reflect.DeepEqual(got, []Protocol{ProtocolMessages}) {
		t.Fatalf("anthropic protocols = %v", got)
	}
}

func TestUpstreamModelsEndpointFollowsProviderFamily(t *testing.T) {
	openai := probeTestProvider("https://gateway.example/v1")
	if got := upstreamModelsEndpoint(openai); got != "https://gateway.example/v1/models" {
		t.Fatalf("openai models endpoint = %q", got)
	}
	anthropic := openai
	anthropic.ProviderType = ProviderFamilyAnthropic
	anthropic.DefaultWireAPI = APIProtocolMessages
	if got := upstreamModelsEndpoint(anthropic); got != "https://gateway.example/v1/models" {
		t.Fatalf("anthropic models endpoint = %q", got)
	}
}

// A base URL may carry a query string — an Azure OpenAI deployment names its
// api-version that way — and the protocol path has to be removed from the URL's
// path rather than from the raw string. With a query present the suffix match
// never hit, so the probe appended /models after the query and asked for a URL
// no deployment serves.
func TestUpstreamModelsEndpointKeepsAQueryString(t *testing.T) {
	openai := probeTestProvider("https://gateway.example/openai/deployments/deployment?api-version=2024-01-01")
	if got := upstreamModelsEndpoint(openai); got != "https://gateway.example/openai/deployments/deployment/v1/models?api-version=2024-01-01" {
		t.Fatalf("configured openai models endpoint = %q", got)
	}
	anthropic := openai
	anthropic.ProviderType = ProviderFamilyAnthropic
	anthropic.DefaultWireAPI = APIProtocolMessages
	if got := upstreamModelsEndpoint(anthropic); got != "https://gateway.example/openai/deployments/deployment/models?api-version=2024-01-01" {
		t.Fatalf("configured anthropic models endpoint = %q", got)
	}
	// The bootstrap scope stores a bare base URL, so its endpoint carries no
	// protocol path yet — and its query is still not part of the models path.
	bootstrap := openai
	bootstrap.Scope = ProviderScopeEnvDefault
	if got := upstreamModelsEndpoint(bootstrap); got != "https://gateway.example/openai/deployments/deployment/models?api-version=2024-01-01" {
		t.Fatalf("bootstrap models endpoint = %q", got)
	}
	// A trailing slash before the query must not hide the protocol path: the raw
	// trim cannot remove a slash that is not at the end of the string, so the
	// segment is removed after the slash is.
	trailing := probeTestProvider("https://gateway.example/v1/responses/?api-version=2024-01-01")
	if got := upstreamModelsEndpoint(trailing); got != "https://gateway.example/v1/models?api-version=2024-01-01" {
		t.Fatalf("already complete endpoint models endpoint = %q", got)
	}
	chat := probeTestProvider("https://gateway.example/v1/chat/completions/?api-version=2024-01-01")
	chat.DefaultWireAPI = APIProtocolChatCompletions
	if got := upstreamModelsEndpoint(chat); got != "https://gateway.example/v1/models?api-version=2024-01-01" {
		t.Fatalf("chat completions endpoint models endpoint = %q", got)
	}
}

func probeTestProvider(baseURL string) Provider {
	return Provider{
		ID:             "gateway",
		Name:           "gateway",
		ProviderType:   ProviderFamilyOpenAI,
		DefaultWireAPI: APIProtocolResponses,
		BaseURL:        baseURL,
		APIKey:         "probe-key",
		AuthHeader:     "Authorization",
		AuthScheme:     "Bearer",
		HeadersJSON:    "{}",
		Weight:         10,
		Enabled:        true,
		Scope:          ProviderScopeCatalog,
	}
}

func writeProbeJSON(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// assertProbeBody pins the probe payload: a one-token "ping" that proves the
// protocol without spending real generation budget.
func assertProbeBody(t *testing.T, raw []byte, contentField string, wantLimit float64) {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("no request body was recorded for %s", contentField)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode probe body: %v", err)
	}
	switch contentField {
	case "input":
		if payload["input"] != "ping" {
			t.Fatalf("input = %v, want ping", payload["input"])
		}
		if payload["max_output_tokens"] != wantLimit {
			t.Fatalf("max_output_tokens = %v, want %v", payload["max_output_tokens"], wantLimit)
		}
	default:
		messages, ok := payload["messages"].([]any)
		if !ok || len(messages) != 1 {
			t.Fatalf("messages = %#v, want one probe message", payload["messages"])
		}
		message, _ := messages[0].(map[string]any)
		if message["content"] != "ping" {
			t.Fatalf("probe content = %v, want ping", message["content"])
		}
		if payload["max_tokens"] != wantLimit {
			t.Fatalf("max_tokens = %v, want %v", payload["max_tokens"], wantLimit)
		}
	}
}
