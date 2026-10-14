package adapters

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	appconfig "github.com/chaitin/agent-compose/pkg/config"
	"github.com/chaitin/agent-compose/pkg/llms"
)

// LLMUpstreamProber verifies which protocols a configured connection actually
// serves and logs the report.
//
// Probing is a diagnostic: no routing decision depends on it, an operator can
// turn it off with LLM_UPSTREAM_PROBE, and every request it issues is bounded by
// the configured timeout. It owns the probe client so the startup sweep and the
// per-write check share one timeout and one log shape.
type LLMUpstreamProber struct {
	enabled bool
	timeout time.Duration
	logger  *slog.Logger
	prober  *llms.UpstreamProber
	// sweepMu guards the handle shutdown uses to end the background startup sweep
	// and to wait for it: sweepCancel ends the context it probes under, and
	// sweepDone is closed by its goroutine. Both are published together before the
	// goroutine starts, so a wait either observes the sweep it must stop — and
	// then waits for that sweep's own goroutine to return — or observes that no
	// sweep exists.
	sweepMu     sync.Mutex
	sweepCancel context.CancelFunc
	sweepDone   chan struct{}
}

// NewLLMUpstreamProber builds the prober from daemon configuration.
func NewLLMUpstreamProber(config *appconfig.Config, logger *slog.Logger) *LLMUpstreamProber {
	if config == nil {
		return &LLMUpstreamProber{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	timeout := config.LLMProbeTimeout
	if timeout <= 0 {
		timeout = llms.DefaultProbeTimeout
	}
	return &LLMUpstreamProber{
		enabled: config.LLMUpstreamProbe,
		timeout: timeout,
		logger:  logger,
		prober:  llms.NewUpstreamProber(&http.Client{Timeout: timeout}),
	}
}

// StartProbeConfigured probes every enabled connection once, after startup has
// materialized the catalog, without delaying the daemon.
//
// The verdicts are diagnostic, so nothing that starts after this call waits on
// an upstream: a slow or unreachable endpoint used to hold recovery and the
// background managers for the whole LLM_PROBE_TIMEOUT budget, and a deployment
// with several such endpoints paid that delay before it served anything. The
// sweep still shares one deadline, so it stops on its own and reports how many
// connections it could not reach; callers that must not outlive it — shutdown —
// stop and wait for it with WaitProbeConfigured.
//
// The returned sweep runs under a context derived from ctx, so the wait can end
// it: the caller's own cancellation is not the only path that must stop it.
func (p *LLMUpstreamProber) StartProbeConfigured(ctx context.Context, store llms.CatalogStore) {
	if p == nil || !p.enabled || store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sweepCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	// The goroutine is started while the handle is held, so a wait can never
	// cancel a sweep whose goroutine has not been started yet.
	p.sweepMu.Lock()
	p.sweepCancel = cancel
	p.sweepDone = done
	go func() {
		defer close(done)
		// Releasing the derived context here keeps its timer and parent link from
		// outliving the sweep.
		defer cancel()
		p.ProbeConfigured(sweepCtx, store)
	}()
	p.sweepMu.Unlock()
}

// WaitProbeConfigured stops a running sweep and waits for it to finish, honoring
// ctx as its deadline. It is what makes the background sweep safe to interrupt:
// shutdown cancels the sweep and then waits here, so the goroutine cannot outlive
// the store it reads.
//
// Cancelling is what keeps the wait short — every request the sweep issues is
// bound to its context — and the deadline is what keeps a sweep that ignores it
// from holding shutdown open. An expired deadline is reported, because the caller
// is about to release the store the sweep is reading.
func (p *LLMUpstreamProber) WaitProbeConfigured(ctx context.Context) error {
	if p == nil {
		return nil
	}
	p.sweepMu.Lock()
	cancel, done := p.sweepCancel, p.sweepDone
	p.sweepMu.Unlock()
	if done == nil {
		return nil
	}
	cancel()
	if ctx == nil {
		<-done
		return nil
	}
	// The wait is on the sweep's own goroutine, so the returned error is the only
	// signal that a sweep outlived the deadline.
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for the llm upstream probe sweep: %w", ctx.Err())
	}
}

// ProbeConfigured probes every enabled connection once and returns when the
// sweep is done. The whole sweep shares one deadline, so a hanging upstream
// cannot delay it indefinitely and the remaining connections are reported
// inconclusive instead.
func (p *LLMUpstreamProber) ProbeConfigured(ctx context.Context, store llms.CatalogStore) {
	if p == nil || !p.enabled || store == nil {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := llms.ProbeConfiguredUpstreams(probeCtx, p.logger, p.prober, store); err != nil {
		p.logger.Warn("llm upstream probe did not complete", "error", err)
	}
}

// ProbeConnection probes one connection, which is what the LLM service calls
// after a provider write. It blocks the write for at most the configured
// timeout, so an operator sees the verdict immediately without the request
// being able to hang.
func (p *LLMUpstreamProber) ProbeConnection(ctx context.Context, provider llms.Provider, model string) {
	if p == nil || !p.enabled {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	llms.ProbeUpstreamAndLog(probeCtx, p.logger, p.prober, provider, model)
}

// Capabilities reports the cached verdict for a connection. It is read-only and
// never probes: a client asking for configuration should not wait on an
// upstream, and reporting "not probed yet" is honest about what the daemon
// knows. Probing disabled means no verdict is ever reported.
func (p *LLMUpstreamProber) Capabilities(provider llms.Provider) (llms.UpstreamProbeResult, bool) {
	if p == nil || !p.enabled || p.prober == nil {
		return llms.UpstreamProbeResult{}, false
	}
	return p.prober.LookupUpstreamProbe(provider)
}
