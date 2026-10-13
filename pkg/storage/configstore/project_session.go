package configstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/chaitin/agent-compose/internal/projects"
	"github.com/chaitin/agent-compose/pkg/idset"
	domain "github.com/chaitin/agent-compose/pkg/model"
)

func (s *projectStore) ListProjectSandboxRuns(ctx context.Context, filter domain.ProjectSandboxRelationFilter) ([]ProjectRunRecord, error) {
	query := projects.SelectProjectRunSQL() + ` WHERE sandbox_id != ''`
	args := make([]any, 0, 4+len(filter.Statuses))
	if projectID := strings.TrimSpace(filter.ProjectID); projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	if agentName := strings.TrimSpace(filter.AgentName); agentName != "" {
		query += ` AND agent_name = ?`
		args = append(args, agentName)
	}
	if sandboxID := strings.TrimSpace(filter.SandboxID); sandboxID != "" {
		query += ` AND sandbox_id = ?`
		args = append(args, sandboxID)
	}
	statuses := projects.NormalizeRunStatusFilter(filter.Statuses)
	if len(statuses) > 0 {
		query += ` AND status IN (` + placeholders(len(statuses)) + `)`
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += ` ORDER BY updated_at DESC, created_at DESC, run_id DESC`
	limit := filter.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	query += ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query project sandbox runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var items []ProjectRunRecord
	for rows.Next() {
		item, err := projects.ScanProjectRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project sandbox runs: %w", err)
	}
	return items, nil
}

func (s *projectStore) ListLatestProjectRunsForSandboxes(ctx context.Context, sandboxIDs []string) (map[string]ProjectRunRecord, error) {
	ids := idset.Normalize(sandboxIDs)
	if len(ids) == 0 {
		return map[string]ProjectRunRecord{}, nil
	}
	result := make(map[string]ProjectRunRecord, len(ids))
	if err := forEachIDBatch(ids, func(batch []string) error {
		query := projects.SelectProjectRunSQL() + ` WHERE sandbox_id IN (` + placeholders(len(batch)) + `) ORDER BY updated_at DESC, created_at DESC, run_id DESC`
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("query latest project runs for sandboxes: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			item, err := projects.ScanProjectRun(rows.Scan)
			if err != nil {
				return err
			}
			if _, exists := result[item.SandboxID]; !exists {
				result[item.SandboxID] = item
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate latest project runs for sandboxes: %w", err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *projectStore) ListProjectRunsForSandbox(ctx context.Context, sandboxID string) ([]ProjectRunRecord, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, fmt.Errorf("sandbox id is required")
	}
	return s.ListProjectSandboxRuns(ctx, domain.ProjectSandboxRelationFilter{SandboxID: sandboxID})
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	values := make([]string, count)
	for i := range values {
		values[i] = "?"
	}
	return strings.Join(values, ",")
}
