package egress

import (
	"strings"
	"testing"
)

func TestRuntimePolicyDriftBetweenReportsOnlyWhatChanged(t *testing.T) {
	mediated := RuntimePolicy{Generation: 11, Application: ApplicationMediated}
	open := RuntimePolicy{Generation: 7, Application: ApplicationOpen}
	applied := RuntimePolicy{Generation: 9, Application: ApplicationDriverApplied}
	unenforced := RuntimePolicy{Generation: 9, Application: ApplicationUnenforced}

	cases := []struct {
		name           string
		previous       RuntimePolicy
		current        RuntimePolicy
		wantChanged    bool
		wantDecided    bool
		wantSummaryHas string
	}{
		{
			name:     "the same policy leaves nothing to report",
			previous: mediated,
			current:  mediated,
		},
		{
			name:        "a mediated sandbox is already decided by the new policy",
			previous:    mediated,
			current:     RuntimePolicy{Generation: 12, Application: ApplicationMediated},
			wantChanged: true,
			wantDecided: true,
			// The proxy reads the declaration per request, so the change already
			// reaches the running sandbox rather than waiting for a recreate.
			wantSummaryHas: "already decided",
		},
		{
			name:           "a runtime created before any record cannot be accounted for",
			previous:       RuntimePolicy{},
			current:        mediated,
			wantChanged:    true,
			wantDecided:    false,
			wantSummaryHas: "cannot be established",
		},
		{
			name:           "an unrecorded current policy claims nothing about the sandbox",
			previous:       mediated,
			current:        RuntimePolicy{},
			wantChanged:    true,
			wantDecided:    false,
			wantSummaryHas: "cannot be accounted for",
		},
		{
			name:           "a runtime created open is not decided by a declaration added later",
			previous:       open,
			current:        mediated,
			wantChanged:    true,
			wantDecided:    false,
			wantSummaryHas: "does not reach it",
		},
		{
			name:           "a driver-applied runtime keeps the declaration it was created with",
			previous:       applied,
			current:        RuntimePolicy{Generation: 10, Application: ApplicationDriverApplied},
			wantChanged:    true,
			wantDecided:    false,
			wantSummaryHas: "until its runtime is recreated",
		},
		{
			name:           "an unenforced runtime was never decided by its own declaration",
			previous:       unenforced,
			current:        mediated,
			wantChanged:    true,
			wantDecided:    false,
			wantSummaryHas: "was not decided by it",
		},
		{
			name:        "two open policies differ without restricting anything",
			previous:    open,
			current:     RuntimePolicy{Generation: 8, Application: ApplicationOpen},
			wantChanged: true,
			wantDecided: true,
			// Both leave egress unrestricted, so there is a difference to record
			// but nothing for a warning to claim.
			wantSummaryHas: "neither the previous nor the current declaration restricts egress",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drift, changed := RuntimePolicyDriftBetween(tc.previous, tc.current)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %t, want %t", changed, tc.wantChanged)
			}
			if !changed {
				if drift != (RuntimePolicyDrift{}) {
					t.Fatalf("drift = %+v, want the zero value when nothing changed", drift)
				}
				return
			}
			if drift.Previous != tc.previous.Generation || drift.Current != tc.current.Generation {
				t.Fatalf("drift generations = %d->%d, want %d->%d", drift.Previous, drift.Current, tc.previous.Generation, tc.current.Generation)
			}
			if drift.DecidedByCurrentPolicy != tc.wantDecided {
				t.Fatalf("decided by the current policy = %t, want %t (%s)", drift.DecidedByCurrentPolicy, tc.wantDecided, drift.Summary)
			}
			if drift.Summary == "" {
				t.Fatal("a reported drift must say what it means")
			}
			if !strings.Contains(drift.Summary, tc.wantSummaryHas) {
				t.Fatalf("summary = %q, want it to contain %q", drift.Summary, tc.wantSummaryHas)
			}
		})
	}
}
