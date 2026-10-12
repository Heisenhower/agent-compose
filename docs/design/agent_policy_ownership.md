# Agent policy ownership and the spec hash

An agent policy declares runtime containment: the outbound network policy a
sandbox enforces and whether a stopped runtime is retained. This note answers one
question about it — does a policy belong to the agent definition, and must it
therefore enter the agent's canonical form and the project spec hash — and
records the semantics the compose schema implements today.

## Three different questions

"What is a policy bound to" usually conflates three separate decisions:

1. **Who owns the constraint.** The project author, or an authority above them
   (an operator or platform that constrains what a project may run).
2. **What the engine enforces.** One effective policy block per sandbox, fixed
   when that sandbox is created.
3. **What the revision hash must cover.** Everything whose change alters
   enforced behavior.

Answering (1) and (2) does not answer (3), and the hash is the only part that the
word "bound" makes concrete.

## An agent definition, and a constraint on it

An agent definition answers *what runs and what it is given*: image and build,
runtime driver, provider and model, system prompt, environment, skills, volumes,
workspace, MCP servers, capset IDs, scheduler. Two projects with the same
definition run the same agent; that is what makes the definition the agent's
identity.

A policy answers *how far the platform lets that agent go*. Two projects with the
same definition and different policies run the same agent under different
containment. Nothing about the agent changes when containment changes: a
tightened `allow` list does not make it a different image, a different provider,
or a different prompt.

**A policy is a constraint on an agent, not a constituent of it.** So the answer
to "must a policy be part of the agent definition" is no. The remainder of this
note is about what that answer does and does not permit.

## What the revision hash is for

The spec hash is not an enforcement mechanism. Enforcement is the per-sandbox
policy snapshot: a sandbox records the policy in effect when it was created, and
editing the project changes only sandboxes created afterwards. The hash has two
other roles (`project-spec-hash-contract.md`):

- **request integrity**: the daemon re-normalizes the submitted spec and compares
  hashes, so client/server normalization drift is reported instead of silently
  applied;
- **revision identity**: a hash that differs from the persisted one creates a new
  project revision, and apply reports the change.

Binding a policy into each agent's canonical JSON therefore buys exactly one
thing: a containment-only edit reads as "these agents changed". It costs the
opposite: a containment-only edit also dirties every affected agent's definition,
and an edit to a shared policy block reads as N unrelated agent changes rather
than one policy change.

The invariant that actually matters is narrower than "everything declared is
hashed":

> A change that alters enforced behavior must alter the revision hash. A change
> that only rewrites sugar the engine resolves away need not.

The compose schema satisfies this. A project `policies` entry is resolved into
each selecting agent's effective policy block during normalization, that
effective block is part of the agent's canonical JSON, and the hash covers it. An
edit to a library entry no agent selects changes nothing the engine enforces, and
is correctly reported as an unchanged project revision.

That last property has a price worth naming: because the library is authoring
sugar that resolves away, the persisted revision records the *resolved* policy of
each agent and not the *declaration* that produced it. An operator reading a
stored revision cannot see which policy an agent selected, and cannot audit the
library as written.

## Semantics implemented today

- `policies` is a catalog, not a layer. An entry is a policy block; an entry
  cannot name another entry.
- An agent selects from it: `policy: <name>` or `policy: <block>`, never both,
  because a scalar and a mapping are different YAML shapes for one key.
- The effective policy is exactly the selected or declared block. There is no
  merge, no partial override, no implicit inheritance, and no project default.
- An unresolved name is an error. An absent declaration means unrestricted, as it
  did before the block existed.

So "project-level" and "agent-level" do not compose today: a library entry takes
effect only through a selection. This is deliberate. A merged block would be a
third block that appears nowhere in the file, and a reader could no longer tell
what a sandbox enforces by reading one declaration.

A project that wants one policy for most agents and one exception is already
expressible — the exception declares its own block, the rest select the entry —
without any composition rule.

## If the two must compose

The composition rule has to follow ownership, not convenience.

- **One owner.** The project author writes both the library and the agent blocks.
  A `default_policy` with override is enough: an agent that omits `policy`
  inherits the default, and an agent that declares one replaces it. Override is
  cheap to reason about, and the author can already write whatever they want.
- **Different owners.** An operator constrains projects it does not author. Then
  override is the wrong rule, because the constrained party must not be able to
  loosen its own constraints; the effective policy is the tightest combination of
  the layers (`deny` wins, allowances intersect), and the report has to say which
  layer decided.

The second case is not reachable by adding a key to `agent-compose.yml`. Today a
project author can delete `network: default: deny` and re-apply; the only thing
preventing an unenforced containment claim from being removed is that the project
file itself is trusted. Genuine operator ownership needs a policy authority
outside the project, its own persistence, an intersection rule, and provenance in
the capability report so a decision can be attributed to a layer. That is a
separate feature, not a schema variant.

## Platform-owned policy: the agreed direction

A policy is the platform's control over what an agent may do at runtime, and the
daemon is what executes it. Most of an agent definition describes the environment
the agent runs in, which the daemon does not interpret; the policy is the part
the daemon must impose regardless of what the agent would prefer, and it is the
part a project author must not be able to relax on their own.

That intent points at ownership the compose file cannot provide: a constraint
that lives in the same file the constrained party edits is a self-declaration,
not a control. Claiming it needs to be able to leave the agent definition, and
with it the agent's version hash, and to be queried and changed through the
daemon instead.

**The precondition is not "remove the field from the hash".** The read path that
enforces a policy today is `ResolveNetworkDeclaration` (`pkg/runs`): it looks up
the managed agent record and decodes the policy out of the agent's canonical
spec JSON. So policy leaves the agent version hash only when three things move
together:

1. the policy becomes a record the daemon owns, with its own generation;
2. the enforcement read path resolves that record instead of the spec JSON;
3. a policy change reaches the daemon even when the project revision is
   unchanged.

Doing (3) last is what breaks silently. A project that adds
`network: {default: deny}` while the policy is outside the hash produces an
unchanged revision, no stored declaration, and an unrestricted sandbox: the
tightening never existed. A field that disappears from the hash without its own
synchronization path is worse than a field that dirties an agent definition.

Two pieces of the mechanism already exist. `pkg/egress` derives a `Generation`
from policy content, and `Result.IsStale(current)` / `Record.IsStale(current)`
already express "this decision was made against a different policy snapshot, so
it must not be acted on". What is missing is a durable policy record, the RPC
surface over it, and a resolver that reads it.

### Rules to settle before implementing

- **Precedence between the file and the daemon record.** Options: the file is
  desired state and re-apply overwrites the record (the record then only matters
  between applies); the record is authoritative and re-apply does not clear it
  unless the declared value itself changed; or the file may only select a policy
  the platform already defines. Recommended: the file keeps declaring, and the
  platform record may only **tighten** it — `deny` wins, allowances intersect —
  so `apply` can never erase a stricter external constraint, and a project
  author cannot widen one.
- **Effect on sandboxes that already exist.** Enforcement is configured by the
  driver when a sandbox is created, so a changed policy cannot retroactively
  constrain a running container without recreating it, which discards its
  private writable layer. A policy change should therefore apply to new sandboxes
  and be *reported* as drift for existing ones (the generation makes this
  expressible), with an explicit reconcile action instead of a silent rebuild.
- **Direction of change.** Whether the platform layer may relax a project
  declaration at all, or only tighten it.

### RPC surface

A new `AgentPolicyService`, alongside `ProjectService`, since a policy is
daemon-owned runtime state and not part of the project revision write path:

- `GetAgentPolicy(project, agent)` → the effective policy, its source
  (`declared` or `platform`), its generation, and when it changed;
- `ListAgentPolicies(project)` → one entry per agent with source and generation;
- `SetAgentPolicy(project, agent, policy, expected_generation)` → optimistic
  concurrency in the pattern `PatchProject.expected_current_spec_hash` already
  establishes, and a generation mismatch fails with `ABORTED`;
- `DeleteAgentPolicy(project, agent)` → returns the agent to its declaration.

The record is keyed by `(project, agent)` and carries a content-derived
generation, the policy itself, and who set it. Creation snapshots that generation
onto the sandbox, which is what makes "which running sandboxes are out of policy"
answerable.

### Delivery order

1. The record, the RPC surface, and a resolver that consults it (falling back to
   the declaration), with policy still inside the spec hash.
2. Once the registry is authoritative, drop the policy from the agent payload and
   the hash, and state plainly in the manuals that a policy change applies to new
   sandboxes.

## Recommendation

1. Keep selection semantics: a name or a block, never a merge, and fail closed on
   an unresolved name. Shipped.
2. Keep the enforced value covered by the revision hash. Shipped — the resolved
   per-agent block is what is hashed.
3. Record the declaration too: keep the library and each agent's selection in the
   normalized spec and in the protobuf, so a containment-only edit reads as one
   policy change, `inspect` can show which policy an agent selected, and a stored
   revision is an audit record of what was declared. This costs a new proto field
   and a YAML-shape round trip for the selection; enforcement is unchanged.
4. Add a project-wide default only if a project-level default is actually wanted,
   and choose override or intersection by the ownership rule above.
