package adapters

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/llms"
)

func TestLLMUpstreamProberSkipsEverythingWhenDisabled(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	store := &probeCatalogStub{providers: []llms.Provider{probeUpstreamProvider(server.URL)}}
	prober := NewLLMUpstreamProber(&appconfig.Config{LLMUpstreamProbe: false}, discardProbeLogger())
	prober.ProbeConnection(context.Background(), probeUpstreamProvider(server.URL), "")
	prober.ProbeConfigured(context.Background(), store)

	if requests.Load() != 0 {
		t.Fatalf("disabled prober issued %d requests", requests.Load())
	}
	if store.listCalls != 0 {
		t.Fatalf("disabled prober read the catalog %d times", store.listCalls)
	}
}

func TestLLMUpstreamProberProbesConfiguredConnectionsAndSingleWrites(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"served-model"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"object":"chat.completion","choices":[{"index":0}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	provider := probeUpstreamProvider(server.URL)
	store := &probeCatalogStub{providers: []llms.Provider{provider}}
	prober := NewLLMUpstreamProber(&appconfig.Config{LLMUpstreamProbe: true, LLMProbeTimeout: 5 * time.Second}, discardProbeLogger())
	prober.ProbeConfigured(context.Background(), store)
	if store.listCalls == 0 {
		t.Fatal("enabled prober did not read the catalog")
	}
	prober.ProbeConnection(context.Background(), provider, "served-model")
}

func TestNewLLMUpstreamProberHandlesMissingConfig(t *testing.T) {
	prober := NewLLMUpstreamProber(nil, nil)
	// A prober built without configuration must not panic or probe.
	prober.ProbeConnection(context.Background(), probeUpstreamProvider("https://gateway.example/v1"), "")
	if prober.enabled {
		t.Fatal("a prober built without configuration must stay disabled")
	}
}

// A configuration that carries no timeout and a probe client built with none
// must fall back to the same budget the daemon documents as the
// LLM_PROBE_TIMEOUT default. When they were separate values, whether the timeout
// was applied at all depended on whether the caller passed a fully materialized
// configuration.
func TestNewLLMUpstreamProberFallsBackToTheConfiguredProbeTimeout(t *testing.T) {
	if llms.DefaultProbeTimeout != appconfig.DefaultLLMProbeTimeout {
		t.Fatalf("probe fallback timeout = %s, want the configured default %s", llms.DefaultProbeTimeout, appconfig.DefaultLLMProbeTimeout)
	}
	prober := NewLLMUpstreamProber(&appconfig.Config{LLMUpstreamProbe: true}, discardProbeLogger())
	if prober.timeout != appconfig.DefaultLLMProbeTimeout {
		t.Fatalf("prober timeout = %s, want %s", prober.timeout, appconfig.DefaultLLMProbeTimeout)
	}
	if prober.prober == nil {
		t.Fatal("prober was not built")
	}
}

// The startup sweep must not delay whatever starts after it. The handler holds
// the first request open for the whole probe budget, so a synchronous sweep could
// not return until that budget expired; a background sweep returns immediately
// and WaitProbeConfigured still observes its completion.
func TestLLMUpstreamProberStartProbeConfiguredDoesNotBlockTheCaller(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce, releaseOnce sync.Once
	releaseSweep := func() { releaseOnce.Do(func() { close(release) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"served-model"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"object":"chat.completion","choices":[{"index":0}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	// Deferred calls run last-in-first-out: Close waits for the outstanding
	// request, so releasing it must be registered after Close to run before it.
	defer server.Close()
	defer releaseSweep()

	store := &probeCatalogStub{providers: []llms.Provider{probeUpstreamProvider(server.URL)}}
	prober := NewLLMUpstreamProber(&appconfig.Config{LLMUpstreamProbe: true, LLMProbeTimeout: time.Hour}, discardProbeLogger())

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		prober.StartProbeConfigured(context.Background(), store)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("StartProbeConfigured did not return while the sweep was still waiting on the upstream")
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the background sweep never reached the upstream")
	}
	if store.listCalls == 0 {
		t.Fatal("the background sweep did not read the catalog")
	}

	releaseSweep()
	prober.WaitProbeConfigured()
}

// Waiting for a sweep that was never started, and starting one on a disabled
// prober, must both be no-ops: shutdown wires the wait unconditionally.
func TestLLMUpstreamProberWaitIsSafeWithoutASweep(t *testing.T) {
	prober := NewLLMUpstreamProber(&appconfig.Config{LLMUpstreamProbe: false}, discardProbeLogger())
	prober.StartProbeConfigured(context.Background(), &probeCatalogStub{})
	prober.WaitProbeConfigured()
	var missing *LLMUpstreamProber
	missing.StartProbeConfigured(context.Background(), &probeCatalogStub{})
	missing.WaitProbeConfigured()
}

type probeCatalogStub struct {
	providers []llms.Provider
	bindings  []llms.ProviderModelBinding
	listCalls int
}

func (s *probeCatalogStub) ListEnabledLLMProviders(context.Context) ([]llms.Provider, error) {
	s.listCalls++
	return s.providers, nil
}

func (s *probeCatalogStub) ListEnabledLLMModels(context.Context) ([]llms.Model, error) {
	return nil, nil
}

func (s *probeCatalogStub) ListLLMProviderModelConfigs(context.Context) ([]llms.ProviderModelBinding, error) {
	return s.bindings, nil
}

func (s *probeCatalogStub) DefaultLLMModelReference(context.Context) (string, string, bool, error) {
	return "", "", false, nil
}

func probeUpstreamProvider(baseURL string) llms.Provider {
	return llms.Provider{
		ID: "gateway", Name: "gateway", ProviderType: llms.ProviderFamilyOpenAI,
		DefaultWireAPI: llms.APIProtocolResponses, BaseURL: baseURL + "/v1", APIKey: "key",
		AuthHeader: "Authorization", AuthScheme: "Bearer", Enabled: true, Scope: llms.ProviderScopeCatalog,
	}
}

func discardProbeLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
