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
