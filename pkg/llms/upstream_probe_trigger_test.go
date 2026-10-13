package llms

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProbeModelIndexPrefersDefaultModel(t *testing.T) {
	bindings := []ProviderModelBinding{
		{ProviderID: "a", ModelID: "zeta"},
		{ProviderID: "a", ModelID: "alpha"},
		{ProviderID: "b", ModelID: "only"},
	}

	withDefault := probeModelIndex(bindings, "a", "default-model", true)
	if want := map[string]string{"a": "default-model", "b": "only"}; !reflect.DeepEqual(withDefault, want) {
		t.Fatalf("with default = %v, want %v", withDefault, want)
	}

	withoutDefault := probeModelIndex(bindings, "", "", false)
	if want := map[string]string{"a": "alpha", "b": "only"}; !reflect.DeepEqual(withoutDefault, want) {
		t.Fatalf("without default = %v, want %v", withoutDefault, want)
	}
}

func TestProbeConfiguredUpstreamsProbesEveryConnection(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"object":"list","data":[{"id":"served-model"}]}`)
		case "/v1/chat/completions":
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := fakeCatalogStore{
		providers:   []Provider{catalogOpenAIConnection("a", server.URL+"/v1"), catalogOpenAIConnection("b", server.URL+"/v1")},
		bindings:    []ProviderModelBinding{{ProviderID: "a", ModelID: "served-model"}},
		defProvider: "a",
		defModel:    "served-model",
		hasDefault:  true,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := ProbeConfiguredUpstreams(context.Background(), logger, NewUpstreamProber(server.Client()), store); err != nil {
		t.Fatalf("ProbeConfiguredUpstreams: %v", err)
	}
	// Each connection gets one inventory request plus one request per protocol
	// its family can serve.
	if got, want := requests.Load(), int64(2*(1+2)); got != want {
		t.Fatalf("probe requests = %d, want %d", got, want)
	}
}

func TestProbeConfiguredUpstreamsStopsWhenTheBudgetEnded(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"object":"list","data":[{"id":"served-model"}]}`)
		case "/v1/chat/completions":
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	store := fakeCatalogStore{
		providers: []Provider{catalogOpenAIConnection("a", server.URL+"/v1"), catalogOpenAIConnection("b", server.URL+"/v1")},
		bindings:  []ProviderModelBinding{{ProviderID: "a", ModelID: "served-model"}},
	}
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ProbeConfiguredUpstreams(ended, logger, NewUpstreamProber(server.Client()), store); err != nil {
		t.Fatalf("ProbeConfiguredUpstreams with an ended budget: %v", err)
	}
	// A spent budget must not cost a request per remaining connection, because
	// each of those would fail before leaving the process.
	if requests.Load() != 0 {
		t.Fatalf("an ended sweep issued %d requests", requests.Load())
	}
	// Nor may it report a verdict per connection: an undecided report the
	// endpoint never earned reads as a finding. The sweep says once that it
	// stopped instead.
	if reports := strings.Count(logged.String(), `msg="llm upstream probe"`); reports != 0 {
		t.Fatalf("an ended sweep logged %d per-connection probe reports: %s", reports, logged.String())
	}
	if !strings.Contains(logged.String(), "stopped before every connection was probed") {
		t.Fatalf("an ended sweep did not report that it stopped: %s", logged.String())
	}
}

// A run-scoped declared connection carries the sandbox's own credential, so the
// startup sweep must not spend it on a diagnostic.
func TestProbeConfiguredUpstreamsSkipsDeclaredConnections(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"object":"list","data":[{"id":"served-model"}]}`)
		case "/v1/chat/completions":
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	declared := catalogOpenAIConnection("run-connection", server.URL+"/v1")
	declared.Scope = ProviderScopeDeclared
	store := fakeCatalogStore{
		providers: []Provider{catalogOpenAIConnection("a", server.URL+"/v1"), declared},
		bindings: []ProviderModelBinding{
			{ProviderID: "a", ModelID: "served-model"},
			{ProviderID: declared.ID, ModelID: "served-model"},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := ProbeConfiguredUpstreams(context.Background(), logger, NewUpstreamProber(server.Client()), store); err != nil {
		t.Fatalf("ProbeConfiguredUpstreams: %v", err)
	}
	if got, want := requests.Load(), int64(1+2); got != want {
		t.Fatalf("probe requests = %d, want %d: the declared connection must not be probed", got, want)
	}
}

// A connection whose declared model has its own endpoint must be probed there.
// The connection endpoint is unreachable in this test, so a sweep that ignored
// the model override would report the connection inconclusive and issue no
// request at all.
func TestProbeConfiguredUpstreamsProbesTheDeclaredModelEndpoint(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/v1/models":
			writeProbeJSON(w, `{"object":"list","data":[{"id":"served-model"}]}`)
		case "/v1/chat/completions":
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	connection := catalogOpenAIConnection("a", "http://127.0.0.1:1/v1")
	binding := ProviderModelBinding{
		ProviderID: connection.ID, ModelID: "served-model",
		Config: ProviderModelConfig{BaseURL: server.URL + "/v1"},
	}
	store := fakeCatalogStore{
		providers:   []Provider{connection},
		bindings:    []ProviderModelBinding{binding},
		defProvider: connection.ID,
		defModel:    "served-model",
		hasDefault:  true,
	}
	prober := NewUpstreamProber(server.Client())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := ProbeConfiguredUpstreams(context.Background(), logger, prober, store); err != nil {
		t.Fatalf("ProbeConfiguredUpstreams: %v", err)
	}
	if got, want := requests.Load(), int64(1+2); got != want {
		t.Fatalf("probe requests = %d, want %d against the model endpoint", got, want)
	}
	verdict, ok := prober.LookupUpstreamProbe(ProviderForModel(connection, []ProviderModelBinding{binding}, "served-model"))
	if !ok {
		t.Fatal("no verdict was cached for the model endpoint")
	}
	if !verdict.Supports(ProtocolChatCompletions) {
		t.Fatalf("verdict = %#v, want the model endpoint's chat_completions support", verdict.Protocols)
	}
}

// A verdict is cached under the endpoint the probe reached, so the sweep has to name
// the same model a capability read resolves. Here the catalog default is not the
// lexicographically first declared model, which is where the two disagreed.
func TestProbeConfiguredUpstreamsProbesTheDefaultModelEndpoint(t *testing.T) {
	var firstHits, defaultHits atomic.Int64
	counting := func(counter *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			counter.Add(1)
			if r.URL.Path == "/v1/models" {
				writeProbeJSON(w, `{"object":"list","data":[{"id":"served-model"}]}`)
				return
			}
			writeProbeJSON(w, `{"object":"chat.completion","choices":[{"index":0}]}`)
		}))
	}
	first, defaultEndpoint := counting(&firstHits), counting(&defaultHits)
	defer first.Close()
	defer defaultEndpoint.Close()

	connection := catalogOpenAIConnection("a", "http://127.0.0.1:1/v1")
	bindings := []ProviderModelBinding{
		{ProviderID: connection.ID, ModelID: "alpha", Config: ProviderModelConfig{BaseURL: first.URL + "/v1"}},
		{ProviderID: connection.ID, ModelID: "gamma", Config: ProviderModelConfig{BaseURL: defaultEndpoint.URL + "/v1"}},
	}
	store := fakeCatalogStore{
		providers: []Provider{connection}, bindings: bindings,
		defProvider: connection.ID, defModel: "gamma", hasDefault: true,
	}
	prober := NewUpstreamProber(defaultEndpoint.Client())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := ProbeConfiguredUpstreams(context.Background(), logger, prober, store); err != nil {
		t.Fatalf("ProbeConfiguredUpstreams: %v", err)
	}
	if got := defaultHits.Load(); got == 0 {
		t.Fatal("the sweep never reached the catalog default model's endpoint")
	}
	if got := firstHits.Load(); got != 0 {
		t.Fatalf("the sweep probed the first declared model's endpoint %d times, want the catalog default", got)
	}

	// A reader resolves the model through the same selector, so it finds the verdict.
	model := ProbeModelFor(connection.ID, bindings, connection.ID, "gamma", true)
	if model != "gamma" {
		t.Fatalf("ProbeModelFor = %q, want the catalog default gamma", model)
	}
	if _, ok := prober.LookupUpstreamProbe(ProviderForModel(connection, bindings, model)); !ok {
		t.Fatal("no verdict is cached under the model a capability read resolves")
	}
}
