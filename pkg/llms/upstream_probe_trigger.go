package llms

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// ProbeUpstreamAndLog probes one connection and writes the report. Every
// trigger — daemon startup and a provider write — goes through it, so the log
// shape and the declared-protocol warning cannot drift apart.
func ProbeUpstreamAndLog(ctx context.Context, logger *slog.Logger, prober *UpstreamProber, provider Provider, model string) {
	if prober == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	result, err := prober.Probe(ctx, UpstreamProbeRequest{Provider: provider, Model: model})
	if err != nil {
		logger.Warn("llm upstream probe could not run", "connection", provider.ID, "error", err)
		return
	}
	logger.Info("llm upstream probe", "connection", provider.ID, "endpoint", result.Endpoint, "model", result.Model, "cached", result.Cached)
	LogUpstreamProbe(logger, result)
	if declared, supported, mismatch := DeclaredProtocolUnsupported(provider, result); mismatch {
		logger.Warn("llm upstream does not support the declared protocol",
			"connection", provider.ID, "endpoint", result.Endpoint,
			"declared", string(declared), "supported", joinProtocols(supported))
	}
}

// ProbeConfiguredUpstreams probes every enabled connection once and logs the
// report. A connection that declares no model is probed with a model the
// endpoint itself advertises, so the check still reaches a real deployment.
//
// The caller bounds the whole sweep through ctx: probes after the deadline fail
// fast, so the sweep stops there and reports how many connections it could not
// reach rather than logging an undecided verdict for each of them.
func ProbeConfiguredUpstreams(ctx context.Context, logger *slog.Logger, prober *UpstreamProber, store CatalogStore) error {
	if prober == nil || store == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	providers, err := store.ListEnabledLLMProviders(ctx)
	if err != nil {
		return fmt.Errorf("list llm connections for probe: %w", err)
	}
	bindings, err := store.ListLLMProviderModelConfigs(ctx)
	if err != nil {
		return fmt.Errorf("list llm model bindings for probe: %w", err)
	}
	defaultProvider, defaultModel, hasDefault, err := store.DefaultLLMModelReference(ctx)
	if err != nil {
		return fmt.Errorf("read the llm default model for probe: %w", err)
	}
	models := probeModelIndex(bindings, defaultProvider, defaultModel, hasDefault)
	probed := 0
	for _, provider := range providers {
		if err := ctx.Err(); err != nil {
			logger.Warn("llm upstream probe stopped before every connection was probed",
				"probed", probed, "skipped", len(providers)-probed, "error", err)
			return nil
		}
		if strings.TrimSpace(provider.Scope) == ProviderScopeDeclared {
			// A run-scoped declared connection carries the sandbox's own credential
			// and exists for that run alone. Probing it would spend a credential the
			// daemon does not own, so the sweep leaves it out.
			continue
		}
		probed++
		model := models[provider.ID]
		ProbeUpstreamAndLog(ctx, logger, prober, ProviderForModel(provider, bindings, model), model)
	}
	return nil
}

// ProbeModelFor picks the model a probe of providerID names: the catalog default
// model when that connection serves it, otherwise the lexicographically first model
// the connection declares. One model is enough to prove an endpoint serves a
// protocol, and its endpoint decides the cache key, so every trigger and every
// capability read has to resolve this the same way or a verdict is written under an
// endpoint the reader never looks up.
func ProbeModelFor(providerID string, bindings []ProviderModelBinding, defaultProvider, defaultModel string, hasDefault bool) string {
	providerID = strings.TrimSpace(providerID)
	if hasDefault && strings.TrimSpace(defaultProvider) == providerID {
		if declared := strings.TrimSpace(defaultModel); declared != "" {
			return declared
		}
	}
	return firstBoundModel(providerID, bindings)
}

// firstBoundModel returns the lexicographically first model providerID declares, or
// "" when it declares none.
func firstBoundModel(providerID string, bindings []ProviderModelBinding) string {
	model := ""
	for _, binding := range bindings {
		if strings.TrimSpace(binding.ProviderID) != providerID {
			continue
		}
		modelID := strings.TrimSpace(binding.ModelID)
		if modelID == "" {
			continue
		}
		if model == "" || modelID < model {
			model = modelID
		}
	}
	return model
}

// probeModelIndex resolves ProbeModelFor for every connection the sweep probes: the
// connections the bindings name, plus the default connection even when it declares
// no model.
func probeModelIndex(bindings []ProviderModelBinding, defaultProvider, defaultModel string, hasDefault bool) map[string]string {
	providers := make([]string, 0, len(bindings)+1)
	for _, binding := range bindings {
		if providerID := strings.TrimSpace(binding.ProviderID); providerID != "" {
			providers = append(providers, providerID)
		}
	}
	if hasDefault {
		if providerID := strings.TrimSpace(defaultProvider); providerID != "" {
			providers = append(providers, providerID)
		}
	}
	models := make(map[string]string, len(providers))
	for _, providerID := range providers {
		if _, ok := models[providerID]; ok {
			continue
		}
		models[providerID] = ProbeModelFor(providerID, bindings, defaultProvider, defaultModel, hasDefault)
	}
	return models
}

func joinProtocols(protocols []Protocol) string {
	if len(protocols) == 0 {
		return "none proven"
	}
	names := make([]string, 0, len(protocols))
	for _, protocol := range protocols {
		names = append(names, string(protocol))
	}
	return strings.Join(names, ", ")
}
