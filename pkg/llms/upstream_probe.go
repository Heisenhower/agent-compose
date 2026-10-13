package llms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
)

const (
	// DefaultProbeTimeout bounds one probe when the caller injects neither a
	// deadline nor a client timeout. It is the daemon's LLM_PROBE_TIMEOUT default
	// rather than a second value of its own: a probe used to fall back to a
	// different budget than a loaded configuration supplied, so whether the
	// timeout was applied depended on whether the caller passed a fully
	// materialized configuration.
	DefaultProbeTimeout = appconfig.DefaultLLMProbeTimeout
	// probeBodyLimit caps how much of a probe response is read and classified.
	probeBodyLimit = 4 << 10
	// probeDetailLimit caps the response excerpt a probe keeps for a log line.
	probeDetailLimit = 200
	// probeModelListLimit caps how many model ids a probe reports.
	probeModelListLimit = 50
)

// ProbeOutcome classifies how one protocol probe ended.
type ProbeOutcome string

const (
	// ProbeSupported means the endpoint served the minimal request over this
	// protocol.
	ProbeSupported ProbeOutcome = "supported"
	// ProbeUnsupported means the endpoint does not serve this protocol.
	ProbeUnsupported ProbeOutcome = "unsupported"
	// ProbeInconclusive means the probe could not decide: a timeout, a server
	// error, a rate limit, or a 2xx body that did not look like the protocol.
	ProbeInconclusive ProbeOutcome = "inconclusive"
	// ProbeAuthFailed means the endpoint rejected the credential. That says
	// nothing about which protocols it serves.
	ProbeAuthFailed ProbeOutcome = "auth_failed"
)

// ProtocolProbe is the result of probing one protocol on one connection.
type ProtocolProbe struct {
	Protocol Protocol
	Outcome  ProbeOutcome
	Detail   string
}

// UpstreamProbeRequest describes the connection to probe and the model to name
// in the minimal requests. The model should be one the connection declares, so
// the probe exercises the deployment a run would actually use.
type UpstreamProbeRequest struct {
	Provider Provider
	Model    string
}

// UpstreamProbeResult reports what one endpoint was observed to serve.
//
// Protocol support is a set, not a choice: one endpoint commonly serves both
// OpenAI protocols, so a probe never stops at the first success.
type UpstreamProbeResult struct {
	Endpoint string
	// Model is the model the protocol probes named. It is the requested model,
	// or one the endpoint advertises when the connection declares none.
	Model        string
	Models       []string
	ModelsDetail string
	Protocols    []ProtocolProbe
	// Cached reports that this verdict was reused instead of re-probed.
	Cached bool
	// ProbedAt is when the verdict was produced, so a reader can tell a fresh
	// verdict from a stale one. A reused verdict keeps its original time.
	ProbedAt time.Time
}

// SupportedProtocols returns the protocols the probe proved are served here.
func (r UpstreamProbeResult) SupportedProtocols() []Protocol {
	var protocols []Protocol
	for _, probe := range r.Protocols {
		if probe.Outcome == ProbeSupported {
			protocols = append(protocols, probe.Protocol)
		}
	}
	return protocols
}

// Supports reports whether the probe proved the endpoint serves protocol.
func (r UpstreamProbeResult) Supports(protocol Protocol) bool {
	outcome, probed := r.Outcome(protocol)
	return probed && outcome == ProbeSupported
}

// Outcome returns the verdict the probe reached for protocol, and whether the
// protocol was probed at all. A caller that reasons about one protocol needs its
// own verdict: another protocol's outcome is not evidence about this one.
func (r UpstreamProbeResult) Outcome(protocol Protocol) (ProbeOutcome, bool) {
	for _, probe := range r.Protocols {
		if probe.Protocol == protocol {
			return probe.Outcome, true
		}
	}
	return "", false
}

// Proven reports whether the probe reached a supported-or-unsupported verdict
// for at least one protocol. An unreachable endpoint proves nothing, and a
// caller must not turn that silence into a claim.
func (r UpstreamProbeResult) Proven() bool {
	for _, probe := range r.Protocols {
		if probe.Outcome == ProbeSupported || probe.Outcome == ProbeUnsupported {
			return true
		}
	}
	return false
}

// UpstreamProbeProtocols returns the protocols worth probing for a connection.
// The probe stays inside the connection's family because that is the family the
// daemon would route to it.
func UpstreamProbeProtocols(provider Provider) []Protocol {
	if NormalizeProviderType(provider.ProviderType) == ProviderFamilyAnthropic {
		return []Protocol{ProtocolMessages}
	}
	return []Protocol{ProtocolChatCompletions, ProtocolResponses}
}

// UpstreamProber probes configured connections over HTTP. It owns the client so
// a caller controls the transport, the proxy, and the timeout, and it owns the
// verdict cache so repeated triggers do not repeat identical traffic.
type UpstreamProber struct {
	client *http.Client
	cache  *UpstreamProbeCache
}

// NewUpstreamProber returns a prober that issues requests through client. A nil
// client, or one without a timeout, gets DefaultProbeTimeout, which is the same
// budget the daemon applies around a probe through LLM_PROBE_TIMEOUT. A caller
// that bounds the probe with its own context deadline still gets that deadline
// first, because it is established before the request starts; the client timeout
// is the backstop for a caller that supplies no deadline at all.
func NewUpstreamProber(client *http.Client) *UpstreamProber {
	if client == nil {
		client = &http.Client{}
	}
	if client.Timeout <= 0 {
		cloned := *client
		cloned.Timeout = DefaultProbeTimeout
		client = &cloned
	}
	return &UpstreamProber{client: client, cache: NewUpstreamProbeCache(ProbeCacheTTL)}
}

// WithProbeCache replaces the verdict cache, which lets a caller choose the
// retention window and lets a test control the clock.
func (p *UpstreamProber) WithProbeCache(cache *UpstreamProbeCache) *UpstreamProber {
	if p != nil {
		p.cache = cache
	}
	return p
}

// LookupUpstreamProbe returns the cached verdict for a connection without
// issuing traffic. It is the read side of the cache: a caller can report the
// most recent capabilities the daemon observed.
func (p *UpstreamProber) LookupUpstreamProbe(provider Provider) (UpstreamProbeResult, bool) {
	if p == nil {
		return UpstreamProbeResult{}, false
	}
	return p.cache.Lookup(UpstreamProbeKey(provider))
}

// Probe lists the connection's models and then probes every protocol its family
// can serve, each independently. The error reports a request the daemon cannot
// even build (no endpoint); an unreachable or refusing upstream is part of the
// result, not an error.
//
// The model-list request runs first, both to log the inventory and to supply a
// model when the connection declares none. A gateway that lists no models still
// gets every protocol probed with the caller's model; only a connection with no
// model on either side is reported inconclusive.
//
// A fresh verdict for the same endpoint and credential is reused instead of
// re-probed, so repeated triggers do not repeat identical upstream traffic.
func (p *UpstreamProber) Probe(ctx context.Context, req UpstreamProbeRequest) (UpstreamProbeResult, error) {
	provider := req.Provider
	result := UpstreamProbeResult{Endpoint: redactedProbeEndpoint(provider.BaseURL)}
	if result.Endpoint == "" {
		return result, fmt.Errorf("probe connection %q: a base url is required", provider.ID)
	}
	cacheKey := UpstreamProbeKey(provider)
	if cached, ok := p.cache.Lookup(cacheKey); ok && probeReuseMatches(cached, req.Model) {
		cached.Cached = true
		return cached, nil
	}
	headers, err := ProviderForwardHeaders(provider)
	if err != nil {
		return result, fmt.Errorf("probe connection %q headers: %w", provider.ID, err)
	}
	result.ProbedAt = time.Now().UTC()
	result.Models, result.ModelsDetail = p.listModels(ctx, provider, headers)
	model := strings.TrimSpace(req.Model)
	if model == "" && len(result.Models) > 0 {
		model = result.Models[0]
	}
	result.Model = model
	protocols := UpstreamProbeProtocols(provider)
	if model == "" {
		detail := inconclusiveModelDetail(ctx, result)
		for _, protocol := range protocols {
			result.Protocols = append(result.Protocols, ProtocolProbe{
				Protocol: protocol,
				Outcome:  ProbeInconclusive,
				Detail:   detail,
			})
		}
		p.storeProbe(ctx, cacheKey, result)
		return result, nil
	}
	for _, protocol := range protocols {
		result.Protocols = append(result.Protocols, p.probeProtocol(ctx, provider, headers, model, protocol))
	}
	p.storeProbe(ctx, cacheKey, result)
	return result, nil
}

// storeProbe caches a verdict for reuse, unless the probe's context already
// ended. A cancelled or exhausted budget says nothing about the endpoint, and
// caching that verdict would let the next trigger reuse an observation that was
// never made, reporting the endpoint as undecided for the whole TTL instead of
// probing it.
func (p *UpstreamProber) storeProbe(ctx context.Context, cacheKey string, result UpstreamProbeResult) {
	if ctx.Err() != nil {
		return
	}
	p.cache.Store(cacheKey, result)
}

// probeReuseMatches reports whether a cached verdict answers the question this
// probe would ask.
//
// The cache key covers the deployment — endpoint, family, headers, credential —
// because protocol support is a property of the endpoint. The verdict itself
// records the model it was taken with and the model list it read, so reusing one
// model's verdict for another reports both wrongly: a connection whose declared
// models changed kept reporting the previous probed_model and inventory for the
// whole TTL, and the write-time probe that was supposed to refresh them returned
// the stale verdict instead.
//
// A caller that names no model asks the endpoint to advertise one, which is the
// same question every time, so any verdict for the deployment answers it.
func probeReuseMatches(cached UpstreamProbeResult, requestedModel string) bool {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" {
		return true
	}
	return strings.TrimSpace(cached.Model) == requestedModel
}

// inconclusiveModelDetail explains why the protocol probes had no model to name.
// Only a model list that was actually read and came back empty supports "the
// endpoint listed none"; an exhausted budget or a failed inventory request means
// the endpoint was never asked, and reporting the empty-list wording there would
// blame a request the daemon never completed.
func inconclusiveModelDetail(ctx context.Context, result UpstreamProbeResult) string {
	switch {
	case ctx.Err() != nil:
		return "the probe budget ended before the endpoint could list a model"
	case result.ModelsDetail != "":
		return "the connection declares no model and the endpoint model list is unavailable: " + result.ModelsDetail
	default:
		return "the connection declares no model and the endpoint listed none"
	}
}

// listModels performs the inventory request. Its outcome never gates the
// protocol probes: a gateway without /v1/models can still serve /responses or
// /chat/completions.
func (p *UpstreamProber) listModels(ctx context.Context, provider Provider, headers http.Header) ([]string, string) {
	endpoint := upstreamModelsEndpoint(provider)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, probeDetail(provider, err.Error())
	}
	copyProbeHeaders(request.Header, headers)
	response, err := p.client.Do(request)
	if err != nil {
		return nil, probeDetail(provider, err.Error())
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, probeBodyLimit))
	if err != nil {
		return nil, probeDetail(provider, "read response: "+err.Error())
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Sprintf("HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "the response is not a model list"
	}
	seen := make(map[string]struct{}, len(payload.Data))
	models := make([]string, 0, len(payload.Data))
	for _, entry := range payload.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	sort.Strings(models)
	if len(models) > probeModelListLimit {
		models = models[:probeModelListLimit]
	}
	return models, ""
}

func (p *UpstreamProber) probeProtocol(ctx context.Context, provider Provider, headers http.Header, model string, protocol Protocol) ProtocolProbe {
	payload, err := probeRequestBody(protocol, model)
	if err != nil {
		return ProtocolProbe{Protocol: protocol, Outcome: ProbeInconclusive, Detail: summariseProbeDetail(err.Error())}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, EndpointForProvider(provider, string(protocol)), bytes.NewReader(payload))
	if err != nil {
		return ProtocolProbe{Protocol: protocol, Outcome: ProbeInconclusive, Detail: probeDetail(provider, err.Error())}
	}
	copyProbeHeaders(request.Header, headers)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.client.Do(request)
	if err != nil {
		return ProtocolProbe{Protocol: protocol, Outcome: ProbeInconclusive, Detail: probeDetail(provider, err.Error())}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, probeBodyLimit))
	if err != nil {
		return ProtocolProbe{Protocol: protocol, Outcome: ProbeInconclusive, Detail: probeDetail(provider, "read response: "+err.Error())}
	}
	return ClassifyProbeResponse(protocol, response.StatusCode, body)
}

// ClassifyProbeResponse maps one HTTP response to a protocol outcome. It is
// pure, so the classification table is testable without a server.
//
// Only a 404/405/501 verdict is treated as proof the protocol is absent. Every
// ambiguous answer stays inconclusive rather than claiming the upstream does not
// support a protocol it may simply have been rate-limiting.
func ClassifyProbeResponse(protocol Protocol, statusCode int, body []byte) ProtocolProbe {
	probe := ProtocolProbe{Protocol: protocol}
	switch {
	case statusCode >= 200 && statusCode < 300:
		if responseMatchesProtocol(protocol, body) {
			probe.Outcome = ProbeSupported
			probe.Detail = "served a " + string(protocol) + " response"
			return probe
		}
		probe.Outcome = ProbeInconclusive
		probe.Detail = fmt.Sprintf("HTTP %d with a body that does not match %s: %s", statusCode, protocol, summariseProbeBody(body))
	case statusCode == http.StatusNotFound || statusCode == http.StatusMethodNotAllowed || statusCode == http.StatusNotImplemented:
		// A 404 can also be the answer to a model the endpoint does not serve
		// rather than to an absent route, and the probe names a model. The body
		// is the only thing that tells the two apart, so a model-level rejection
		// claims nothing rather than reporting the protocol as absent.
		if statusCode == http.StatusNotFound && probeBodyNamesAMissingModel(body) {
			probe.Outcome = ProbeInconclusive
			probe.Detail = fmt.Sprintf("HTTP %d reported a model the endpoint does not serve: %s", statusCode, summariseProbeBody(body))
			return probe
		}
		probe.Outcome = ProbeUnsupported
		probe.Detail = fmt.Sprintf("HTTP %d", statusCode)
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		probe.Outcome = ProbeAuthFailed
		probe.Detail = fmt.Sprintf("HTTP %d", statusCode)
	case statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity:
		// The request was well formed and reached a handler for this path, so a
		// semantic rejection means the protocol is served here.
		probe.Outcome = ProbeSupported
		probe.Detail = fmt.Sprintf("HTTP %d: %s", statusCode, summariseProbeBody(body))
	default:
		probe.Outcome = ProbeInconclusive
		probe.Detail = fmt.Sprintf("HTTP %d", statusCode)
	}
	return probe
}

// DeclaredProtocolUnsupported reports the mismatch worth surfacing: the
// connection declares a protocol the probe proved this endpoint cannot serve.
//
// Only the declared protocol's own verdict is evidence about it. A sibling
// protocol succeeding says nothing about this one, and an inconclusive outcome
// is not a verdict at all, so the report stays false unless the probe reached
// ProbeUnsupported for the declared protocol itself. That keeps the promise the
// classification table makes: only a 404/405/501 verdict proves absence.
func DeclaredProtocolUnsupported(provider Provider, result UpstreamProbeResult) (declared Protocol, supported []Protocol, mismatch bool) {
	declared = NormalizeProtocol(provider.DefaultWireAPI)
	if !declared.Valid() {
		return declared, result.SupportedProtocols(), false
	}
	outcome, probed := result.Outcome(declared)
	return declared, result.SupportedProtocols(), probed && outcome == ProbeUnsupported
}

// LogUpstreamProbe writes the probe report in the operator-facing form used by
// every trigger: the model list first, then one line per protocol.
func LogUpstreamProbe(logger *slog.Logger, result UpstreamProbeResult) {
	if logger == nil {
		logger = slog.Default()
	}
	if result.ModelsDetail != "" {
		logger.Warn("endpoint model list unavailable", "endpoint", result.Endpoint, "detail", result.ModelsDetail)
	} else {
		logger.Info("endpoint supports model list", "endpoint", result.Endpoint, "models", strings.Join(result.Models, ", "))
	}
	for _, probe := range result.Protocols {
		switch probe.Outcome {
		case ProbeSupported:
			logger.Info("endpoint supports protocol", "endpoint", result.Endpoint, "protocol", string(probe.Protocol), "detail", probe.Detail)
		case ProbeUnsupported:
			logger.Warn("endpoint does not support protocol", "endpoint", result.Endpoint, "protocol", string(probe.Protocol), "detail", probe.Detail)
		case ProbeAuthFailed:
			logger.Warn("endpoint rejected the credential; protocol support is unknown", "endpoint", result.Endpoint, "protocol", string(probe.Protocol))
		default:
			logger.Warn("endpoint protocol probe is inconclusive", "endpoint", result.Endpoint, "protocol", string(probe.Protocol), "detail", probe.Detail)
		}
	}
}

// probeRequestBody builds the smallest request that still proves the protocol:
// one user message and a one-token completion limit.
func probeRequestBody(protocol Protocol, model string) ([]byte, error) {
	messages := []map[string]string{{"role": "user", "content": "ping"}}
	var payload any
	switch protocol {
	case ProtocolChatCompletions:
		payload = map[string]any{"model": model, "messages": messages, "max_tokens": 1, "stream": false}
	case ProtocolMessages:
		payload = map[string]any{"model": model, "messages": messages, "max_tokens": 1}
	default:
		payload = map[string]any{"model": model, "input": "ping", "max_output_tokens": 1, "stream": false}
	}
	return json.Marshal(payload)
}

// responseMatchesProtocol guards against a gateway that answers 200 to
// everything: a success only counts when the body looks like the protocol that
// was asked for.
func responseMatchesProtocol(protocol Protocol, body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return false
	}
	var payload struct {
		Object  *string          `json:"object"`
		Type    *string          `json:"type"`
		Output  *json.RawMessage `json:"output"`
		Content *json.RawMessage `json:"content"`
		Choices *json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	// A present field is enough to recognize the shape; the values are not
	// inspected because a probe deliberately asks for the smallest possible
	// response.
	switch protocol {
	case ProtocolChatCompletions:
		return payload.Choices != nil || (payload.Object != nil && *payload.Object == "chat.completion")
	case ProtocolMessages:
		return payload.Content != nil || (payload.Type != nil && *payload.Type == "message")
	default:
		return payload.Output != nil || (payload.Object != nil && *payload.Object == "response")
	}
}

// upstreamModelsEndpoint derives /models from the endpoint one protocol uses,
// so it follows the same base-path normalization as a real call.
//
// The protocol path is removed from the URL's path instead of from the raw
// string. A base URL may carry a query string — an Azure OpenAI deployment names
// its api-version that way — and a suffix match against the whole URL never hits
// then, which appended /models after the query and probed a URL no deployment
// serves. The query survives the rewrite, because it is part of the deployment's
// address rather than of the protocol path.
func upstreamModelsEndpoint(provider Provider) string {
	protocol := ProtocolResponses
	if NormalizeProviderType(provider.ProviderType) == ProviderFamilyAnthropic {
		protocol = ProtocolMessages
	}
	// A trailing slash is removed before the protocol segment is, because a base
	// URL whose query string follows the slash keeps it past the raw trim and
	// would otherwise leave "/responses" unrecognized.
	stripProtocolPath := func(endpointPath string) string {
		endpointPath = strings.TrimRight(endpointPath, "/")
		for _, suffix := range []string{"/responses", "/chat/completions", "/messages"} {
			if strings.HasSuffix(endpointPath, suffix) {
				return strings.TrimSuffix(endpointPath, suffix)
			}
		}
		return endpointPath
	}
	endpoint := strings.TrimRight(EndpointForProvider(provider, string(protocol)), "/")
	parsed, err := url.Parse(endpoint)
	if err != nil {
		// EndpointForProvider builds its result from parsed URLs, so this is a
		// malformed configuration rather than a shape to reason about: ask for
		// the models path under whatever the operator typed.
		return stripProtocolPath(endpoint) + "/models"
	}
	cleanPath := stripProtocolPath(parsed.Path)
	if cleanPath == "" {
		cleanPath = "/"
	}
	parsed.Path = path.Join(cleanPath, "models")
	// The original path may have been percent-encoded; the rewritten one is not,
	// so drop the raw form and let String re-encode the path it now holds.
	parsed.RawPath = ""
	return parsed.String()
}

func copyProbeHeaders(destination, source http.Header) {
	for key, values := range source {
		for _, value := range values {
			destination.Add(key, value)
		}
	}
}

// summariseProbeBody collapses a response body into one bounded, single-line
// excerpt. It carries no request headers, so no credential reaches the log.
func summariseProbeBody(body []byte) string {
	return summariseProbeDetail(string(body))
}

// probeBodyNamesAMissingModel reports whether a response body is a gateway's
// model-not-found error. The probe names a model it took from the connection or
// from the endpoint's own model list, so a rejection that names the model says
// nothing about whether the protocol is served.
func probeBodyNamesAMissingModel(body []byte) bool {
	lowered := strings.ToLower(string(body))
	for _, marker := range []string{
		"model_not_found", "model not found", "unknown model", "no such model", "model does not exist",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// redactedProbeEndpoint removes the credential-bearing parts of a connection's
// stored base URL before the probe reports it. Only the API-managed write path
// rejects userinfo and a query string; a connection materialized from the daemon
// environment or from models.json may carry either, and neither belongs in a log.
func redactedProbeEndpoint(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if parsed, err := url.Parse(trimmed); err == nil {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		parsed.RawFragment = ""
		return parsed.String()
	}
	// Not a URL that can be rebuilt: keep what identifies the endpoint and drop
	// whatever a userinfo, query, or fragment could carry.
	redacted := trimProbeUserinfo(trimmed)
	if index := strings.IndexAny(redacted, "?#"); index >= 0 {
		redacted = redacted[:index]
	}
	return redacted
}

// probeDetail renders one probe failure for a report. The transport quotes the
// URL it called — with the query string, and with the userinfo Go keeps while
// masking only the password — so the connection's own credential-bearing parts
// are removed from the excerpt as well.
func probeDetail(provider Provider, detail string) string {
	return summariseProbeDetail(redactProbeURL(detail, provider.BaseURL))
}

// redactProbeURL removes the parts of one stored base URL from a report string.
// It is a text-level scrub because the URL reaches the detail inside a transport
// error, already reassembled with the protocol path appended and, for the
// password, replaced by Go's own redaction.
func redactProbeURL(value, baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return value
	}
	if userinfo := probeUserinfo(trimmed); userinfo != "" {
		value = strings.ReplaceAll(value, userinfo+"@", "")
		if username, _, found := strings.Cut(userinfo, ":"); found {
			value = strings.ReplaceAll(value, username+":***@", "")
		}
	}
	if query := probeQuery(trimmed); query != "" {
		value = strings.ReplaceAll(value, "?"+query, "")
	}
	return value
}

// probeUserinfo returns the "user:password" segment of a base URL, or "" when it
// carries none. A value the parser rejects is read textually, because such a URL
// still reaches the transport error this scrubs.
func probeUserinfo(baseURL string) string {
	if parsed, err := url.Parse(baseURL); err == nil {
		if parsed.User == nil {
			return ""
		}
		return parsed.User.String()
	}
	authority := baseURL
	if scheme := strings.Index(authority, "//"); scheme >= 0 {
		authority = authority[scheme+2:]
	}
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return ""
	}
	return authority[:at]
}

// trimProbeUserinfo removes a "user:password@" segment from a value the URL
// parser could not read.
func trimProbeUserinfo(value string) string {
	userinfo := probeUserinfo(value)
	if userinfo == "" {
		return value
	}
	return strings.Replace(value, userinfo+"@", "", 1)
}

// probeQuery returns the query string of a base URL without its "?" prefix.
func probeQuery(baseURL string) string {
	if parsed, err := url.Parse(baseURL); err == nil {
		return parsed.RawQuery
	}
	index := strings.Index(baseURL, "?")
	if index < 0 {
		return ""
	}
	query := baseURL[index+1:]
	if fragment := strings.Index(query, "#"); fragment >= 0 {
		query = query[:fragment]
	}
	return query
}

func summariseProbeDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > probeDetailLimit {
		detail = detail[:probeDetailLimit] + "..."
	}
	return detail
}
