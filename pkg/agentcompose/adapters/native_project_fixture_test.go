package adapters

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chaitin/agent-compose/internal/projects"
	"github.com/chaitin/agent-compose/pkg/compose"
	domain "github.com/chaitin/agent-compose/pkg/model"
	"github.com/chaitin/agent-compose/pkg/storage/configstore"
)

func createNativeTestAgent(t testing.TB, ctx context.Context, store *configstore.ConfigStore, definition domain.AgentDefinition) domain.AgentDefinition {
	t.Helper()
	projectID := "test-project-" + definition.ID
	agentName := strings.TrimSpace(definition.AgentName)
	if agentName == "" {
		agentName = definition.Name
	}
	agent := compose.NormalizedAgentSpec{
		Name: agentName, Enabled: definition.Enabled, DisplayName: definition.Name,
		Description: definition.Description, Provider: definition.Provider, Model: definition.Model,
		SystemPrompt: definition.SystemPrompt, Image: definition.GuestImage,
		Env: adapterTestEnvSpec(definition.EnvItems), CapsetIDs: append([]string(nil), definition.CapsetIDs...),
	}
	if configJSON := strings.TrimSpace(definition.ConfigJSON); configJSON != "" {
		var config struct {
			Policy *compose.NormalizedSandboxSpec `json:"policy"`
		}
		if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
			t.Fatalf("unmarshal native agent fixture config: %v", err)
		}
		agent.Policy = config.Policy
	}
	if driver := strings.TrimSpace(definition.Driver); driver != "" {
		agent.Driver = &compose.NormalizedDriverSpec{Name: driver}
	}
	specJSON, err := (&compose.NormalizedProjectSpec{Name: "test-project", Agents: []compose.NormalizedAgentSpec{agent}}).MarshalCanonicalJSON(false)
	if err != nil {
		t.Fatalf("marshal native agent fixture: %v", err)
	}
	project, err := store.UpsertProject(ctx, domain.ProjectRecord{ID: projectID, Name: "test-project"})
	if err != nil {
		t.Fatalf("upsert native agent project: %v", err)
	}
	revision, _, err := store.SaveProjectRevision(ctx, domain.ProjectRevisionRecord{ProjectID: project.ID, SpecHash: "fixture-" + definition.ID, SpecJSON: string(specJSON)})
	if err != nil {
		t.Fatalf("save native agent revision: %v", err)
	}
	agentID, err := projects.StableProjectAgentID(project.ID, agentName)
	if err != nil {
		t.Fatalf("derive native project agent id: %v", err)
	}
	if _, err := store.UpsertProjectAgent(ctx, domain.ProjectAgentRecord{ID: agentID, ProjectID: project.ID, AgentName: agentName, Revision: revision.Revision, SpecJSON: "{}"}); err != nil {
		t.Fatalf("upsert native project agent: %v", err)
	}
	loaded, err := store.GetAgentDefinition(ctx, agentID)
	if err != nil {
		t.Fatalf("load native agent definition: %v", err)
	}
	return loaded
}

func createNativeTestScheduler(t testing.TB, ctx context.Context, store *configstore.ConfigStore, scheduler domain.Scheduler) domain.Scheduler {
	t.Helper()
	projectID := "test-project-" + scheduler.Summary.ID
	agentName := "worker"
	agent := compose.NormalizedAgentSpec{
		Name: agentName, Enabled: true, Provider: scheduler.Summary.DefaultAgent, Image: scheduler.Summary.GuestImage,
		Env: adapterTestEnvSpec(scheduler.EnvItems), CapsetIDs: append([]string(nil), scheduler.Summary.CapsetIDs...),
		Scheduler: &compose.NormalizedSchedulerSpec{
			Enabled: scheduler.Summary.Enabled, SandboxPolicy: scheduler.Summary.SandboxPolicy,
			ConcurrencyPolicy: scheduler.Summary.ConcurrencyPolicy, DisplayName: scheduler.Summary.Name,
			Description: scheduler.Summary.Description, Script: scheduler.Script,
		},
	}
	if workspaceID := strings.TrimSpace(scheduler.Summary.WorkspaceID); workspaceID != "" {
		agent.Workspace = &compose.WorkspaceSpec{Name: workspaceID}
	}
	if driver := strings.TrimSpace(scheduler.Summary.Driver); driver != "" {
		agent.Driver = &compose.NormalizedDriverSpec{Name: driver}
	}
	specJSON, err := (&compose.NormalizedProjectSpec{Name: "test-project", Agents: []compose.NormalizedAgentSpec{agent}}).MarshalCanonicalJSON(false)
	if err != nil {
		t.Fatalf("marshal native scheduler fixture: %v", err)
	}
	project, err := store.UpsertProject(ctx, domain.ProjectRecord{ID: projectID, Name: "test-project"})
	if err != nil {
		t.Fatalf("upsert native scheduler project: %v", err)
	}
	revision, _, err := store.SaveProjectRevision(ctx, domain.ProjectRevisionRecord{ProjectID: project.ID, SpecHash: "fixture-" + scheduler.Summary.ID, SpecJSON: string(specJSON)})
	if err != nil {
		t.Fatalf("save native scheduler revision: %v", err)
	}
	if _, err := store.UpsertProjectAgent(ctx, domain.ProjectAgentRecord{ProjectID: project.ID, AgentName: agentName, Revision: revision.Revision, SchedulerEnabled: true, SpecJSON: "{}"}); err != nil {
		t.Fatalf("upsert scheduler project agent: %v", err)
	}
	if _, err := store.UpsertProjectScheduler(ctx, domain.ProjectSchedulerRecord{ID: scheduler.Summary.ID, ProjectID: project.ID, SchedulerID: scheduler.Summary.ID, AgentName: agentName, Revision: revision.Revision, Enabled: scheduler.Summary.Enabled, TriggerCount: len(scheduler.Triggers), SpecJSON: "{}"}); err != nil {
		t.Fatalf("upsert native scheduler: %v", err)
	}
	if _, err := store.ReplaceSchedulerTriggers(ctx, scheduler.Summary.ID, scheduler.Triggers); err != nil {
		t.Fatalf("replace native scheduler triggers: %v", err)
	}
	loaded, err := store.GetScheduler(ctx, scheduler.Summary.ID)
	if err != nil {
		t.Fatalf("load native scheduler: %v", err)
	}
	return loaded
}

func adapterTestEnvSpec(items []domain.SandboxEnvVar) map[string]compose.EnvVarSpec {
	result := make(map[string]compose.EnvVarSpec, len(items))
	for _, item := range items {
		result[item.Name] = compose.EnvVarSpec{Value: item.Value, Secret: item.Secret}
	}
	return result
}
