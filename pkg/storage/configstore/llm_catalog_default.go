package configstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/chaitin/agent-compose/pkg/llms"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

// projectCatalogDefault applies the `default` key of one models.json projection
// and reports the effective reference it could not leave in place, if any.
//
// Three cases. An absent key (nil) leaves the stored reference alone, so a default
// an operator chose through the LLM service survives every restart and a later
// file can restore a declaration it dropped. A present but empty value is the file
// declaring that it has no default, which clears the stored reference. Any other
// value must name a connection the file declares and enables.
//
// A declaration the store cannot serve is not a reason to refuse the whole file —
// every other declaration in it is valid, and refusing would keep the daemon from
// starting — so it is reported instead. The stored reference is only removed when
// it cannot resolve either: the file naming an id it does not control says nothing
// about whether the reference already stored is usable. Only a store failure is
// returned as an error.
func projectCatalogDefault(ctx context.Context, tx *sql.Tx, reference *string, now int64) (*llms.CatalogDefaultDropped, error) {
	if reference == nil {
		return nil, nil
	}
	declared := strings.TrimSpace(*reference)
	if declared == "" {
		previous, ok, err := storedCatalogDefault(ctx, tx)
		if err != nil {
			return nil, err
		}
		if err := clearCatalogDefault(ctx, tx); err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
		return &llms.CatalogDefaultDropped{Stored: previous, Reason: "models.json declares no default"}, nil
	}
	if err := applyCatalogDefault(ctx, tx, declared, now); err != nil {
		if !errors.Is(err, domain.ErrInvalidArgument) {
			return nil, err
		}
		return refuseCatalogDefault(ctx, tx, declared, err)
	}
	return nil, nil
}

// refuseCatalogDefault handles a `default` the file declared but the store cannot
// serve.
func refuseCatalogDefault(ctx context.Context, tx *sql.Tx, declared string, cause error) (*llms.CatalogDefaultDropped, error) {
	previous, ok, err := storedCatalogDefault(ctx, tx)
	if err != nil {
		return nil, err
	}
	if ok {
		resolves, err := storedDefaultResolves(ctx, tx, previous)
		if err != nil {
			return nil, err
		}
		if resolves {
			return &llms.CatalogDefaultDropped{Declared: declared, Reason: cause.Error()}, nil
		}
	}
	if err := clearCatalogDefault(ctx, tx); err != nil {
		return nil, err
	}
	return &llms.CatalogDefaultDropped{Declared: declared, Stored: previous, Reason: cause.Error()}, nil
}

// storedDefaultResolves reports whether the connection a stored reference names is
// still enabled, which is what makes the reference usable as the default.
//
// The model does not have to be one the connection enumerates: the catalog
// resolves a default by naming its connection outright and keeps the model opaque,
// so a literal model is served with the connection's defaults. Requiring a binding
// here deleted an operator's working default whenever the file declared one it
// could not apply.
func storedDefaultResolves(ctx context.Context, tx *sql.Tx, reference string) (bool, error) {
	providerID, _, ok := llms.SplitProviderModelReference(reference)
	if !ok {
		return false, nil
	}
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM llm_provider WHERE id = ? AND enabled != 0`, providerID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("read whether the stored llm default resolves: %w", err)
	}
	return count > 0, nil
}

// storedCatalogDefault reads the effective default reference, or reports that the
// store holds none.
func storedCatalogDefault(ctx context.Context, tx *sql.Tx) (string, bool, error) {
	var providerID, modelID string
	err := tx.QueryRowContext(ctx, `SELECT provider_id, model_id FROM llm_catalog_default WHERE singleton = 1`).Scan(&providerID, &modelID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the stored llm default: %w", err)
	}
	providerID, modelID = strings.TrimSpace(providerID), strings.TrimSpace(modelID)
	if providerID == "" || modelID == "" {
		return "", false, nil
	}
	return providerID + "/" + modelID, true, nil
}

func clearCatalogDefault(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM llm_catalog_default`); err != nil {
		return fmt.Errorf("clear model catalog default: %w", err)
	}
	return nil
}

func applyCatalogDefault(ctx context.Context, tx *sql.Tx, reference string, now int64) error {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		// projectCatalogDefault decides what an empty declaration means; nothing is
		// stored from one.
		return nil
	}
	providerID, modelID, ok := llms.SplitProviderModelReference(reference)
	if !ok {
		return fmt.Errorf("%w: models.json default %q is not a <provider>/<model> reference", domain.ErrInvalidArgument, reference)
	}
	// The default must name a connection models.json itself declared and that can
	// actually serve a request. A reference to a connection this file lost control
	// of, or to one without a credential, would otherwise be stored as the
	// effective default while resolution ignores it: GetDefaultModel would report
	// a default no run receives, and a run that declares no model would be routed
	// by the connection fallback instead.
	var scope, protocol string
	var enabled int
	err := tx.QueryRowContext(ctx, `SELECT scope, enabled, default_wire_api FROM llm_provider WHERE id = ?`, providerID).Scan(&scope, &enabled, &protocol)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: models.json default %q names connection %q, which models.json does not declare", domain.ErrInvalidArgument, reference, providerID)
	}
	if err != nil {
		return fmt.Errorf("resolve model catalog default provider %q: %w", providerID, err)
	}
	// A released row (unset scope) is a connection a previous models.json declared
	// and this one does not: the row survives so a later file can restore it, but it
	// cannot serve the default this file is asking for.
	owner := strings.TrimSpace(scope)
	if owner == "" {
		return fmt.Errorf("%w: models.json default %q names connection %q, which models.json does not declare", domain.ErrInvalidArgument, reference, providerID)
	}
	if owner != llms.ProviderScopeCatalog {
		return fmt.Errorf("%w: models.json default %q names connection %q, which scope %q owns", domain.ErrInvalidArgument, reference, providerID, owner)
	}
	if enabled == 0 {
		return fmt.Errorf("%w: models.json default %q names connection %q, which is disabled: declare an apiKey, or \"enabled\": true for a connection that needs no credential", domain.ErrInvalidArgument, reference, providerID)
	}
	if err := ensureCatalogModelIdentity(ctx, tx, modelID, now); err != nil {
		return fmt.Errorf("upsert model catalog default model: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO llm_provider_model(
		provider_id, model_id, wire_api, weight, base_url, headers_json, max_output_tokens, display_name)
		VALUES(?, ?, ?, 10, '', '{}', 0, '')
		ON CONFLICT(provider_id, model_id) DO NOTHING`, providerID, modelID, protocol); err != nil {
		return fmt.Errorf("bind model catalog default: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM llm_catalog_default`); err != nil {
		return fmt.Errorf("clear model catalog default: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO llm_catalog_default(singleton, provider_id, model_id, updated_at) VALUES(1, ?, ?, ?)`, providerID, modelID, now); err != nil {
		return fmt.Errorf("store model catalog default: %w", err)
	}
	return nil
}
