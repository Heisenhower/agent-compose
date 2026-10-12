package compose

import "sort"

// The agent policy block was renamed from `sandbox` to `policy`. The old key is
// still accepted, because the stable authoring contract records
// `agents.*.sandbox` and because specs and agent definition payloads persisted
// by an earlier engine carry it. A declared policy read as absent would silently
// become an undeclared one, which the engine treats as unrestricted, so every
// reader resolves the block through these accessors instead of reading a field
// directly.
//
// Normalization writes `policy` only, so the deprecated key disappears from
// rewritten specs and remains a read path for existing data. Everything the
// engine enforces is the normalized policy block; the deprecated key exists only
// at the authoring boundary and is reported there.

// legacyPolicyKey is the deprecated spelling of the policy block. It is the key
// the stable authoring contract records, so it keeps working; see the
// deprecation note above.
const legacyPolicyKey = "sandbox"

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

// DeprecatedPolicyBlock names an agent that still declares its policy block
// under the deprecated `sandbox` key.
type DeprecatedPolicyBlock struct {
	// Agent is the agent path, "agents.<name>".
	Agent string
	// Path is the deprecated key that was used, "agents.<name>.sandbox".
	Path string
	// Current is where the declaration belongs, "agents.<name>.policy".
	Current string
}

// DeprecatedPolicyBlocks returns the agents that declared their policy block
// under the deprecated `sandbox` key, ordered by agent name.
//
// It reads the authoring spec rather than a normalized one, because
// normalization resolves both spellings into the policy block and the deprecated
// key is deliberately not carried any further. A caller that wants to report the
// deprecation therefore has to ask before normalizing, which is also the only
// point where the project's own text is still in hand.
func DeprecatedPolicyBlocks(spec *ProjectSpec) []DeprecatedPolicyBlock {
	if spec == nil {
		return nil
	}
	names := make([]string, 0, len(spec.Agents))
	for name := range spec.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	var blocks []DeprecatedPolicyBlock
	for _, name := range names {
		if spec.Agents[name].Sandbox == nil {
			continue
		}
		path := joinPath("agents", name)
		blocks = append(blocks, DeprecatedPolicyBlock{
			Agent:   path,
			Path:    path + "." + legacyPolicyKey,
			Current: path + ".policy",
		})
	}
	return blocks
}
