package adapters

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	driverpkg "github.com/chaitin/agent-compose/pkg/driver"
	"github.com/chaitin/agent-compose/pkg/egress"
	"github.com/chaitin/agent-compose/pkg/llms"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

// egressProxyCredentialSource marks the credential the engine issues for its
// forward proxy. The credential is stored with the sandbox's other facade
// tokens, so it is revoked with the sandbox, but only a credential carrying this
// source authenticates a proxy request: the sandbox's LLM facade token is a
// different authorization and is not accepted in its place.
const egressProxyCredentialSource = "egress-proxy"

// EgressProxyCredentialStore is the sandbox-scoped credential store the proxy
// credential is issued into. It is already the store the daemon revokes per
// sandbox, so a retired sandbox cannot keep proxy access.
type EgressProxyCredentialStore interface {
	SaveLLMFacadeToken(context.Context, llms.FacadeToken) error
	GetLLMFacadeToken(context.Context, string) (llms.FacadeToken, error)
}

// SandboxByIDLoader loads a sandbox so the proxy can decide against the
// declaration that sandbox has *now*, not the one it had when it started.
type SandboxByIDLoader interface {
	GetSandbox(context.Context, string) (*domain.Sandbox, error)
}

// SandboxEgressProxy is the engine-side mediation between a sandbox and the
// internet: it issues the credential the guest presents to the forward proxy,
// builds the environment that routes the guest through it, answers the proxy's
// authentication, and hands the proxy the policy to decide with. It is what the
// daemon passes to the proxy as both its authorizer and its policy source.
//
// Its zero value is inert, and it is inert whenever the deployment cannot offer
// a proxy the sandbox can reach (no configured runtime base URL, or a daemon
// listening only on the host loopback while the sandbox runs in its own network
// namespace). An inert value adds no environment and issues no credential, which
// is what keeps an undeclared or unenforceable sandbox on its existing behavior.
type SandboxEgressProxy struct {
	config          *appconfig.Config
	credentials     EgressProxyCredentialStore
	sandboxes       SandboxByIDLoader
	networkPolicies driverSandboxNetworkPolicy
}

// NewSandboxEgressProxy builds the mediation from the daemon's own dependencies.
// It is pure: whether a sandbox actually gets a proxy is decided per sandbox by
// available, because reachability depends on that sandbox's address.
func NewSandboxEgressProxy(
	config *appconfig.Config,
	credentials EgressProxyCredentialStore,
	sandboxes SandboxByIDLoader,
	declarations SandboxNetworkDeclarationResolver,
) SandboxEgressProxy {
	return SandboxEgressProxy{
		config:          config,
		credentials:     credentials,
		sandboxes:       sandboxes,
		networkPolicies: driverSandboxNetworkPolicy{config: config, declarations: declarations},
	}
}

// available reports whether this deployment can route a sandbox through the
// proxy. It is the single condition that decides whether the engine may claim
// mediation, so the environment, the credential, and the enforcement report
// cannot disagree.
func (p SandboxEgressProxy) available(session *domain.Sandbox) bool {
	if p.credentials == nil || p.sandboxes == nil || session == nil {
		return false
	}
	if strings.TrimSpace(session.Summary.ID) == "" {
		return false
	}
	return egressProxyAddress(p.config, session) != ""
}

// Enable issues the sandbox's proxy credential and returns the environment that
// routes its HTTP traffic through the engine's forward proxy. It returns no
// environment and no error when this deployment cannot offer a reachable proxy,
// because a sandbox that cannot use the proxy must keep running rather than be
// given a proxy address it cannot reach.
func (p SandboxEgressProxy) Enable(ctx context.Context, session *domain.Sandbox) ([]driverpkg.SandboxEnvVar, error) {
	if !p.available(session) {
		return nil, nil
	}
	rawCredential, token, err := llms.NewFacadeToken(llms.NewFacadeTokenRequest{
		SandboxID: session.Summary.ID,
		Source:    egressProxyCredentialSource,
	})
	if err != nil {
		return nil, fmt.Errorf("mint egress proxy credential for sandbox %s: %w", session.Summary.ID, err)
	}
	if err := p.credentials.SaveLLMFacadeToken(ctx, token); err != nil {
		return nil, fmt.Errorf("store egress proxy credential for sandbox %s: %w", session.Summary.ID, err)
	}
	proxyURL := (&url.URL{
		Scheme: "http",
		Host:   egressProxyAddress(p.config, session),
		User:   url.UserPassword(session.Summary.ID, rawCredential),
	}).String()
	return egressProxyEnvItems(proxyURL, egressProxyBypass(p.config, session)), nil
}

// AuthorizeEgressProxy answers the proxy's authentication: which sandbox does
// this credential belong to. It rejects a credential that was not issued for the
// proxy, one that has been revoked or expired, and one presented under a
// different sandbox's name.
func (p SandboxEgressProxy) AuthorizeEgressProxy(ctx context.Context, claimedSandboxID, credential string) (string, error) {
	if p.credentials == nil {
		return "", errors.New("the egress proxy credential store is not configured")
	}
	if strings.TrimSpace(credential) == "" {
		return "", errors.New("no egress proxy credential was presented")
	}
	token, err := p.credentials.GetLLMFacadeToken(ctx, credential)
	if err != nil {
		return "", fmt.Errorf("resolve egress proxy credential: %w", err)
	}
	if token.Source != egressProxyCredentialSource {
		return "", fmt.Errorf("credential source %q is not %q", token.Source, egressProxyCredentialSource)
	}
	if strings.TrimSpace(token.SandboxID) == "" {
		return "", errors.New("egress proxy credential names no sandbox")
	}
	if !token.RevokedAt.IsZero() {
		return "", errors.New("egress proxy credential was revoked")
	}
	if !token.ExpiresAt.IsZero() && !time.Now().UTC().Before(token.ExpiresAt.UTC()) {
		return "", errors.New("egress proxy credential expired")
	}
	claimed := strings.TrimSpace(claimedSandboxID)
	if claimed != "" && claimed != token.SandboxID {
		return "", errors.New("egress proxy credential belongs to another sandbox")
	}
	return token.SandboxID, nil
}

// EgressProxyPolicy returns the policy the proxy decides one sandbox's
// connections with. It loads the sandbox per request so the decision follows the
// declaration the project has now, which is what lets a policy change reach a
// sandbox that is already running.
func (p SandboxEgressProxy) EgressProxyPolicy(ctx context.Context, sandboxID string) (egress.Policy, error) {
	if p.sandboxes == nil {
		return egress.Policy{}, errors.New("the egress proxy cannot load sandboxes")
	}
	session, err := p.sandboxes.GetSandbox(ctx, strings.TrimSpace(sandboxID))
	if err != nil {
		return egress.Policy{}, fmt.Errorf("load sandbox %s for the egress decision: %w", sandboxID, err)
	}
	declaration, engineEndpoints, err := p.networkPolicies.inputs(ctx, session)
	if err != nil {
		return egress.Policy{}, err
	}
	policy, err := egress.EffectiveNetworkPolicy(declaration, engineEndpoints)
	if err != nil {
		return egress.Policy{}, fmt.Errorf("compile the egress policy for sandbox %s: %w", sandboxID, err)
	}
	return policy, nil
}

// egressProxyAddress is the host:port the sandbox must use to reach the proxy:
// the same guest-reachable address the engine hands the sandbox for its runtime
// LLM facade, because that is the address the engine already knows works from
// inside the sandbox. An empty result means no proxy is reachable.
func egressProxyAddress(config *appconfig.Config, session *domain.Sandbox) string {
	base := strings.TrimSpace(llms.GuestRuntimeBaseURL(config, session))
	if base == "" {
		return ""
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

// egressProxyBypass lists the hosts the guest must not send through the proxy:
// the engine's own endpoints, which the policy auto-allows anyway, so routing
// them through the proxy would add a hop and a failure mode for no decision.
func egressProxyBypass(config *appconfig.Config, session *domain.Sandbox) []string {
	hosts := []string{"127.0.0.1", "localhost"}
	if address := egressProxyAddress(config, session); address != "" {
		if host, _, err := net.SplitHostPort(address); err == nil {
			hosts = append(hosts, host)
		}
	}
	if config != nil {
		if telemetry := strings.TrimSpace(config.AgentTelemetry.Endpoint); telemetry != "" {
			if parsed, err := url.Parse(telemetry); err == nil && parsed.Hostname() != "" {
				hosts = append(hosts, parsed.Hostname())
			}
		}
	}
	return hosts
}

// egressProxyEnvItems renders the proxy environment. Both cases are set because
// clients disagree about which they read, and the value is marked secret so a
// credential embedded in it is never echoed by a listing or a log.
func egressProxyEnvItems(proxyURL string, bypass []string) []driverpkg.SandboxEnvVar {
	noProxy := strings.Join(bypass, ",")
	items := []driverpkg.SandboxEnvVar{
		{Name: "HTTP_PROXY", Value: proxyURL, Secret: true},
		{Name: "HTTPS_PROXY", Value: proxyURL, Secret: true},
		{Name: "NO_PROXY", Value: noProxy, Secret: true},
		{Name: "http_proxy", Value: proxyURL, Secret: true},
		{Name: "https_proxy", Value: proxyURL, Secret: true},
		{Name: "no_proxy", Value: noProxy, Secret: true},
	}
	return items
}
