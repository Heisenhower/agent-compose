package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/chaitin/agent-compose/pkg/llms"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

func TestRuntimeLLMFacadeRewritesGrokIntegralFloats(t *testing.T) {
	upstream := `{"object":"response","output":[{"type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"go test\",\"timeout_ms\":66121.0,\"yield_time_ms\":2500.0,\"ratio\":1.5}"}]}`
	rec := serveRuntimeLLM(t, "grok-4.7", &fakeRuntimeLLMHTTPClient{status: http.StatusOK, body: upstream})
	got := rec.Body.String()
	if rec.Code != http.StatusOK || strings.Contains(got, "66121.0") || strings.Contains(got, "2500.0") || !strings.Contains(got, "66121") || !strings.Contains(got, "1.5") {
		t.Fatalf("grok response status=%d body=%s", rec.Code, got)
	}

	other := serveRuntimeLLM(t, "gpt-5.5", &fakeRuntimeLLMHTTPClient{status: http.StatusOK, body: upstream})
	if other.Code != http.StatusOK || !strings.Contains(other.Body.String(), "66121.0") {
		t.Fatalf("non-grok response status=%d body=%s", other.Code, other.Body.String())
	}
}

func serveRuntimeLLM(t *testing.T, model string, client *fakeRuntimeLLMHTTPClient) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	RegisterRuntimeLLMFacadeRoutes(e, RuntimeLLMOptions{
		Tokens:      fakeRuntimeLLMTokens{token: llms.FacadeToken{SandboxID: "sandbox-1", Model: model, ProviderID: "provider-1", WireAPI: llms.APIProtocolResponses, ExpiresAt: time.Now().Add(time.Hour)}},
		Sandboxes:   fakeRuntimeLLMSessions{session: &domain.Sandbox{Summary: domain.SandboxSummary{ID: "sandbox-1", VMStatus: domain.VMStatusRunning}}},
		Connections: fakeRuntimeLLMTargetResolver("http://upstream.test/v1"),
		Client:      client,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/runtime/sandboxes/sandbox-1/llm/openai/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer raw-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}
