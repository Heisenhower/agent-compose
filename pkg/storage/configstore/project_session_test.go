package configstore

import (
	"context"
	"fmt"
	"testing"
)

func TestListLatestProjectRunsForSandboxesBatchesLargeInput(t *testing.T) {
	store := FromDB(newMemoryDB(t))
	if err := store.initSchema(context.Background()); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	ids := make([]string, sqliteIDQueryBatchSize*66)
	for i := range ids {
		ids[i] = fmt.Sprintf("sandbox-%05d", i)
	}
	if _, err := store.ListLatestProjectRunsForSandboxes(context.Background(), ids); err != nil {
		t.Fatalf("large sandbox ID query: %v", err)
	}
}

func TestListProjectAgentsByIDsBatchesLargeInput(t *testing.T) {
	store := FromDB(newMemoryDB(t))
	if err := store.initSchema(context.Background()); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	ids := make([]string, sqliteIDQueryBatchSize*66)
	for i := range ids {
		ids[i] = fmt.Sprintf("agent-%05d", i)
	}
	if _, err := store.ListProjectAgentsByIDs(context.Background(), ids); err != nil {
		t.Fatalf("large managed agent ID query: %v", err)
	}
}
