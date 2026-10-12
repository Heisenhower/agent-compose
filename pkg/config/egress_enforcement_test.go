package config

import "testing"

func TestParseEgressUnenforceablePolicy(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    EgressUnenforceablePolicy
		wantErr bool
	}{
		{name: "unset keeps the default", raw: "", want: EgressUnenforceableWarn},
		{name: "explicit warn", raw: "warn", want: EgressUnenforceableWarn},
		{name: "explicit refuse", raw: "refuse", want: EgressUnenforceableRefuse},
		{name: "surrounding space and case are tolerated", raw: " Refuse ", want: EgressUnenforceableRefuse},
		{name: "an unknown mode is rejected instead of defaulting", raw: "block", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseEgressUnenforceablePolicy(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseEgressUnenforceablePolicy(%q) error = nil, want an error", test.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseEgressUnenforceablePolicy(%q) error = %v", test.raw, err)
			}
			if got != test.want {
				t.Fatalf("ParseEgressUnenforceablePolicy(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

// TestEgressUnenforceablePolicyDefaultIsWarn pins the default: an unenforceable
// declaration must not make an agent unrunnable, and a deployment that never set
// the variable gets that behavior rather than a refusal.
func TestEgressUnenforceablePolicyDefaultIsWarn(t *testing.T) {
	if EgressUnenforceableWarn != "warn" {
		t.Fatalf("EgressUnenforceableWarn = %q, want %q", EgressUnenforceableWarn, "warn")
	}
	t.Setenv(EgressUnenforceablePolicyName, "")
	got, err := loadEgressUnenforceablePolicy()
	if err != nil {
		t.Fatalf("loadEgressUnenforceablePolicy() error = %v", err)
	}
	if got != EgressUnenforceableWarn {
		t.Fatalf("loadEgressUnenforceablePolicy() = %q, want the warn default", got)
	}
}
