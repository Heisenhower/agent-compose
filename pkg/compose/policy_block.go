package compose

// The agent policy block was renamed from `sandbox` to `policy`. The old key is
// still accepted, because the stable authoring contract records
// `agents.*.sandbox` and because specs and agent definition payloads persisted
// by an earlier engine carry it. A declared policy read as absent would silently
// become an undeclared one, which the engine treats as unrestricted, so every
// reader resolves the block through these accessors instead of reading a field
// directly.
//
// Normalization writes `policy` only, so the deprecated key disappears from
// rewritten specs and remains a read path for existing data.

// PolicyBlock returns the policy block a parsed agent declared, preferring
// `policy` over its deprecated `sandbox` spelling.
func (a AgentSpec) PolicyBlock() *SandboxSpec {
	if a.Policy != nil {
		return a.Policy
	}
	return a.Sandbox
}

// PolicyBlock returns the policy block a normalized agent carries, preferring
// `policy` over its deprecated `sandbox` spelling.
func (n NormalizedAgentSpec) PolicyBlock() *NormalizedSandboxSpec {
	if n.Policy != nil {
		return n.Policy
	}
	return n.Sandbox
}
