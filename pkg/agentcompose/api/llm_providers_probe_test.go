package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/chaitin/agent-compose/pkg/llms"
	"github.com/chaitin/agent-compose/pkg/storage/configstore"
	storagesqlite "github.com/chaitin/agent-compose/pkg/storage/sqlite"
	agentcomposev2 "github.com/chaitin/agent-compose/proto/agentcompose/v2"
)

// A provider write must hand the saved connection to the probe, so an operator
// learns the protocol verdict at configuration time rather than at the first
// request.
func TestProviderWriteRunsTheUpstreamProbe(t *testing.T) {
	ctx := context.Background()
	handler, recorder := newProbedLLMHandler(t)

	spec := &agentcomposev2.LLMProviderSpec{
		Id: "gateway", BaseUrl: "https://example.com/v1", Protocol: "responses", ApiKey: proto.String("upstream-secret"),
	}
	if _, err := handler.CreateProvider(ctx, connect.NewRequest(&agentcomposev2.CreateProviderRequest{Provider: spec})); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	probed := recorder.last()
	if probed.ID != "gateway" || probed.DefaultWireAPI != llms.APIProtocolResponses {
		t.Fatalf("created connection probed as %#v", probed)
	}

	spec.ApiKey = nil
	spec.Protocol = "chat_completions"
	if _, err := handler.UpdateProvider(ctx, connect.NewRequest(&agentcomposev2.UpdateProviderRequest{Provider: spec})); err != nil {
		t.Fatalf("UpdateProvider: %v", err)
	}
	probed = recorder.last()
	if probed.ID != "gateway" || probed.DefaultWireAPI != llms.APIProtocolChatCompletions {
		t.Fatalf("updated connection probed as %#v", probed)
	}
	if got := recorder.count(); got != 2 {
		t.Fatalf("probe calls = %d, want one per write", got)
	}
}

// A handler without a prober must still serve provider writes.
func TestProviderWriteWithoutProberSucceeds(t *testing.T) {
	ctx := context.Background()
	handler := newLLMTestHandler(t)
	spec := &agentcomposev2.LLMProviderSpec{Id: "gateway", BaseUrl: "https://example.com/v1", Protocol: "responses", ApiKey: proto.String("upstream-secret")}
	if _, err := handler.CreateProvider(ctx, connect.NewRequest(&agentcomposev2.CreateProviderRequest{Provider: spec})); err != nil {
		t.Fatalf("CreateProvider without prober: %v", err)
	}
}

// A write is committed before its response is described, so a failed model-list
// read must not be reported as a failed mutation: a client that retried the
// Create would hit ErrAlreadyExists for configuration that was applied.
func TestProviderWriteReportsSuccessWhenTheModelListReadFails(t *testing.T) {
	ctx := context.Background()
	store := &failingBindingStore{LLMProviderStore: newLLMTestStore(t)}
	handler := NewLLMHandler(nil, store)
	spec := &agentcomposev2.LLMProviderSpec{
		Id: "gateway", BaseUrl: "https://example.com/v1", Protocol: "responses", ApiKey: proto.String("upstream-secret"),
		Models: &agentcomposev2.LLMProviderModels{Models: []*agentcomposev2.LLMModelSpec{{Id: "declared-model"}}},
	}

	store.fail = true
	response, err := handler.CreateProvider(ctx, connect.NewRequest(&agentcomposev2.CreateProviderRequest{Provider: spec}))
	if err != nil {
		t.Fatalf("CreateProvider error = %v, want the committed write reported", err)
	}
	written := response.Msg.GetProvider()
	if written.GetId() != "gateway" || len(written.GetModels()) != 0 {
		t.Fatalf("response = %#v, want the connection reported without a model list", written)
	}

	// The write itself succeeded, so a read that works reports the declared model.
	store.fail = false
	read, err := handler.GetProvider(ctx, connect.NewRequest(&agentcomposev2.GetProviderRequest{Id: "gateway"}))
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if models := read.Msg.GetProvider().GetModels(); len(models) != 1 || models[0].GetId() != "declared-model" {
		t.Fatalf("stored models = %#v, want the declared model", models)
	}
}

// A stored model the response cannot carry must not turn a committed write into a
// failed one either: the connection is reported without its model list.
func TestProviderWriteReportsSuccessWhenAModelCannotBeReported(t *testing.T) {
	ctx := context.Background()
	store := &failingBindingStore{
		LLMProviderStore: newLLMTestStore(t),
		bindings: []llms.ProviderModelBinding{
			{ProviderID: "gateway", ModelID: "alpha", Config: llms.ProviderModelConfig{HeadersJSON: "{not-json"}},
		},
	}
	handler := NewLLMHandler(nil, store)
	spec := &agentcomposev2.LLMProviderSpec{Id: "gateway", BaseUrl: "https://example.com/v1", Protocol: "responses", ApiKey: proto.String("upstream-secret")}

	response, err := handler.CreateProvider(ctx, connect.NewRequest(&agentcomposev2.CreateProviderRequest{Provider: spec}))
	if err != nil {
		t.Fatalf("CreateProvider error = %v, want the committed write reported", err)
	}
	written := response.Msg.GetProvider()
	if written.GetId() != "gateway" || len(written.GetModels()) != 0 {
		t.Fatalf("response = %#v, want the connection reported without a model list", written)
	}
}

// A connection the write disabled serves nothing until it is enabled again, so
// the write does not wait out a probe budget for it.
func TestProviderWriteDoesNotProbeADisabledConnection(t *testing.T) {
	ctx := context.Background()
	handler, recorder := newProbedLLMHandler(t)
	spec := &agentcomposev2.LLMProviderSpec{
		Id: "gateway", BaseUrl: "https://example.com/v1", Protocol: "responses",
		ApiKey: proto.String("upstream-secret"), Enabled: proto.Bool(false),
	}
	if _, err := handler.CreateProvider(ctx, connect.NewRequest(&agentcomposev2.CreateProviderRequest{Provider: spec})); err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	if got := recorder.count(); got != 0 {
		t.Fatalf("probe calls = %d, want none for a disabled connection", got)
	}
}

// A verdict belongs to the endpoint the probe reached, so a connection whose
// declared model overrides the base url must have its capabilities read from that
// model endpoint. Reading the cache with the connection reported none for exactly
// the connections whose models each have their own endpoint.
func TestProviderResponsesReportCapabilitiesFromTheProbedModelEndpoint(t *testing.T) {
	ctx := context.Background()
	store := newLLMTestStore(t)
	modelEndpoint := "https://model.example/v1"
	recorder := &recordingUpstreamProber{cachedByBaseURL: map[string]llms.UpstreamProbeResult{
		modelEndpoint: {
			Endpoint: modelEndpoint, Model: "deployed",
			Protocols: []llms.ProtocolProbe{{Protocol: llms.APIProtocolResponses, Outcome: llms.ProbeSupported}},
		},
	}}
	handler := NewLLMHandler(nil, store).WithUpstreamProbe(recorder)
	spec := &agentcomposev2.LLMProviderSpec{
		Id: "gateway", BaseUrl: "https://connection.example/v1", Protocol: "responses", ApiKey: proto.String("upstream-secret"),
		Models: &agentcomposev2.LLMProviderModels{Models: []*agentcomposev2.LLMModelSpec{
			{Id: "deployed", BaseUrl: modelEndpoint},
		}},
	}

	created, err := handler.CreateProvider(ctx, connect.NewRequest(&agentcomposev2.CreateProviderRequest{Provider: spec}))
	if err != nil {
		t.Fatalf("CreateProvider: %v", err)
	}
	assertProbedModelCapabilities(t, "the write response", created.Msg.GetProvider())

	read, err := handler.GetProvider(ctx, connect.NewRequest(&agentcomposev2.GetProviderRequest{Id: "gateway"}))
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	assertProbedModelCapabilities(t, "the read response", read.Msg.GetProvider())

	listed, err := handler.ListProviders(ctx, connect.NewRequest(&agentcomposev2.ListProvidersRequest{}))
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	if providers := listed.Msg.GetProviders(); len(providers) != 1 {
		t.Fatalf("listed providers = %#v, want one", providers)
	} else {
		assertProbedModelCapabilities(t, "the list response", providers[0])
	}

	if got := recorder.last().BaseURL; got != modelEndpoint {
		t.Fatalf("probed endpoint = %q, want the declared model endpoint %q", got, modelEndpoint)
	}
	for _, endpoint := range recorder.capabilityEndpoints() {
		if endpoint != modelEndpoint {
			t.Fatalf("capabilities read for %q, want the probed model endpoint %q", endpoint, modelEndpoint)
		}
	}
}

func assertProbedModelCapabilities(t *testing.T, source string, provider *agentcomposev2.LLMProvider) {
	t.Helper()
	capabilities := provider.GetCapabilities()
	if capabilities == nil {
		t.Fatalf("%s reports no capabilities, want the probed model endpoint's verdict", source)
	}
	if capabilities.GetProbedModel() != "deployed" {
		t.Fatalf("%s capabilities = %#v, want the verdict for the probed model", source, capabilities)
	}
}

// failingBindingStore fails the model list a write is described with, while the
// write itself still reaches the store underneath. A test can also replace the
// list the store returns to describe a stored model the response cannot carry.
type failingBindingStore struct {
	LLMProviderStore
	fail     bool
	bindings []llms.ProviderModelBinding
}

func (s *failingBindingStore) ListLLMProviderModelConfigs(ctx context.Context) ([]llms.ProviderModelBinding, error) {
	switch {
	case s.fail:
		return nil, errors.New("model list unavailable")
	case s.bindings != nil:
		return s.bindings, nil
	default:
		return s.LLMProviderStore.ListLLMProviderModelConfigs(ctx)
	}
}

// newLLMTestHandler builds a handler over an in-memory configuration store,
// which is the shape every provider write test needs.
func newLLMTestHandler(t *testing.T) *LLMHandler {
	t.Helper()
	return NewLLMHandler(nil, newLLMTestStore(t))
}

// newProbedLLMHandler adds a recording prober so a test can observe what the
// write path handed to the probe.
func newProbedLLMHandler(t *testing.T) (*LLMHandler, *recordingUpstreamProber) {
	t.Helper()
	recorder := &recordingUpstreamProber{}
	return NewLLMHandler(nil, newLLMTestStore(t)).WithUpstreamProbe(recorder), recorder
}

func newLLMTestStore(t *testing.T) *configstore.ConfigStore {
	t.Helper()
	db, err := storagesqlite.Open(":memory:", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return configstore.FromDB(db.DB())
}

type probeCall struct {
	provider llms.Provider
	model    string
}

type recordingUpstreamProber struct {
	mu     sync.Mutex
	calls  []probeCall
	cached map[string]llms.UpstreamProbeResult
	// cachedByBaseURL answers the way the real prober does: a verdict belongs to
	// the endpoint that was probed, not to the connection that reached it.
	cachedByBaseURL map[string]llms.UpstreamProbeResult
	capabilityCalls []llms.Provider
}

func (r *recordingUpstreamProber) ProbeConnection(_ context.Context, provider llms.Provider, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, probeCall{provider: provider, model: model})
}

func (r *recordingUpstreamProber) Capabilities(provider llms.Provider) (llms.UpstreamProbeResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capabilityCalls = append(r.capabilityCalls, provider)
	if result, ok := r.cachedByBaseURL[provider.BaseURL]; ok {
		return result, true
	}
	result, ok := r.cached[provider.ID]
	return result, ok
}

// capabilityEndpoints reports the endpoints capabilities were read for.
func (r *recordingUpstreamProber) capabilityEndpoints() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	endpoints := make([]string, 0, len(r.capabilityCalls))
	for _, provider := range r.capabilityCalls {
		endpoints = append(endpoints, provider.BaseURL)
	}
	return endpoints
}

func (r *recordingUpstreamProber) last() llms.Provider {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return llms.Provider{}
	}
	return r.calls[len(r.calls)-1].provider
}

func (r *recordingUpstreamProber) lastModel() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return ""
	}
	return r.calls[len(r.calls)-1].model
}

func (r *recordingUpstreamProber) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}
