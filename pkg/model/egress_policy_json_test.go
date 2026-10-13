package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestVMStateOmitsAnUnrecordedEgressPolicy pins the persisted shape. A struct
// field ignores omitempty, so the key was written even when nothing was
// recorded; omitzero writes it only when there is a fact to carry, and a record
// that is absent decodes to the same zero value a reader already treats as "no
// record".
func TestVMStateOmitsAnUnrecordedEgressPolicy(t *testing.T) {
	encoded, err := json.Marshal(VMState{})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "egress_policy") {
		t.Fatalf("encoded state = %s, want no egress_policy key for an unrecorded policy", encoded)
	}

	recorded := VMState{EgressPolicy: SandboxEgressPolicy{Generation: 7, Applied: "open"}}
	encoded, err = json.Marshal(recorded)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(encoded), `"egress_policy":{"generation":7,"applied":"open"}`) {
		t.Fatalf("encoded state = %s, want the recorded egress policy", encoded)
	}
}
