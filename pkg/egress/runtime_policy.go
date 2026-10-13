package egress

// Application records whether — and by what mechanism — the effective policy was
// applied to a sandbox when its runtime was created. A generation alone cannot
// answer this: an open policy and a declaration that no mechanism could apply
// leave the same sandbox in completely different places, and the difference is
// exactly what a later reader of the record needs.
type Application string

const (
	// ApplicationUnrecorded means the engine has no record of the policy this
	// runtime was created under: the runtime predates this record, or it was
	// created by a path that does not resolve a declaration.
	ApplicationUnrecorded Application = ""
	// ApplicationOpen means the policy in force at creation restricted nothing,
	// so the runtime was created with unrestricted egress.
	ApplicationOpen Application = "open"
	// ApplicationDriverApplied means the runtime driver itself applied the
	// declaration when it created the runtime. The driver reads the declaration
	// at creation, so a retained runtime keeps the one it was created with.
	ApplicationDriverApplied Application = "driver-applied"
	// ApplicationUnenforced means the declaration denied by default but no
	// mechanism applied it, and the deployment chose to let the sandbox run
	// (EGRESS_UNENFORCEABLE_POLICY=warn).
	ApplicationUnenforced Application = "declared-unenforced"
	// ApplicationMediated means the engine's forward proxy decides this
	// sandbox's traffic and reads the policy on every request.
	ApplicationMediated Application = "mediated"
)

// RuntimePolicy is the egress fact recorded for one sandbox runtime: the
// generation of the policy that was in force when the runtime was created, and
// how that policy reached it.
type RuntimePolicy struct {
	Generation  Generation
	Application Application
}

// Recorded reports whether this is a record rather than the zero value.
func (p RuntimePolicy) Recorded() bool {
	return p.Application != ApplicationUnrecorded
}

// RuntimePolicyDrift is the answer to "is the sandbox that is running decided by
// the policy that is in force now?".
type RuntimePolicyDrift struct {
	Previous Generation
	Current  Generation
	// DecidedByCurrentPolicy reports whether traffic from the running sandbox is
	// already decided by the policy in force now.
	DecidedByCurrentPolicy bool
	// Summary states what changed and what it means for the sandbox that is
	// running. It is one sentence because it is logged as one line.
	Summary string
}

// RuntimePolicyDriftBetween compares the policy a sandbox's runtime was created
// under with the one in force now. The second result is false when the two
// agree, so a caller reports only a real difference.
//
// The distinction it draws is the one a generation cannot: whether the change
// already reaches the running sandbox. A mediator that reads the declaration per
// request has already applied it; a driver that applied the declaration when it
// created the runtime, a runtime created with unrestricted egress, and an
// unrecorded runtime have not, and only recreating the runtime will.
func RuntimePolicyDriftBetween(previous, current RuntimePolicy) (RuntimePolicyDrift, bool) {
	if previous == current {
		return RuntimePolicyDrift{}, false
	}
	drift := RuntimePolicyDrift{Previous: previous.Generation, Current: current.Generation}
	switch {
	case !current.Recorded():
		// Nothing may be claimed about the policy in force, so nothing may be
		// claimed about whether it reaches the sandbox.
		drift.Summary = "the engine could not record the egress policy now in force, so this sandbox's egress cannot be accounted for"
	case !previous.Recorded():
		drift.Summary = "this sandbox's runtime was created before the engine recorded its egress policy, so whether the policy now in force decides this sandbox cannot be established"
	case previous.Application == ApplicationMediated:
		drift.DecidedByCurrentPolicy = true
		drift.Summary = "the declared egress policy changed since this sandbox's runtime was created; the engine's forward proxy reads the declaration on every request, so this sandbox is already decided by the one in force"
	case current.Application == ApplicationOpen && previous.Application == ApplicationOpen:
		// Both policies leave egress unrestricted, so there is a difference to
		// record but nothing to warn about.
		drift.DecidedByCurrentPolicy = true
		drift.Summary = "the declared egress policy changed since this sandbox's runtime was created, but neither the previous nor the current declaration restricts egress"
	case previous.Application == ApplicationDriverApplied:
		drift.Summary = "the declared egress policy changed since this sandbox's runtime was created; the runtime driver applied the previous declaration when it created the runtime, so this sandbox keeps it until its runtime is recreated"
	case previous.Application == ApplicationUnenforced:
		drift.Summary = "the declaration in force when this sandbox's runtime was created was never applied (EGRESS_UNENFORCEABLE_POLICY=warn), so this sandbox's egress was not decided by it and is not decided by the one in force now"
	default:
		// The runtime was created with unrestricted egress and something now
		// declares a restriction: this is the case where a running sandbox keeps
		// an access the declaration denies.
		drift.Summary = "this sandbox's runtime was created without an egress policy, and the declaration now in force does not reach it: a sandbox that is already running keeps the egress it started with until its runtime is recreated"
	}
	return drift, true
}
