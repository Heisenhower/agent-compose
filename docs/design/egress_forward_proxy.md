# Egress forward proxy

## Problem

A project can declare `policy.network`, but on three of four drivers the engine
installs no filter at all: Docker's only network knob is the daemon's network
mode, Kubernetes emits no egress `NetworkPolicy`, and BoxLite's host-side binding
is unverified. Until now the gap was papered over by refusing to start a sandbox
that declared `default: deny`, which turned a restriction into an availability
failure. The interim behavior is now `warn`: the sandbox runs and the daemon
reports the declaration as unenforced (`EGRESS_UNENFORCEABLE_POLICY=refuse` keeps
the fail-closed mode). That makes the gap honest, not smaller. This document
closes it.

## Decision: enforce in an engine-side forward proxy

The engine already terminates guest traffic: the runtime LLM facade and the
capset gateway are HTTP services the sandbox reaches inside the daemon. A forward
proxy on that same boundary is the enforcement point because it is the only place
that is simultaneously

- available on every driver whose guest can reach the daemon (Docker, Kubernetes,
  BoxLite, Microsandbox alike), so the declaration stops depending on driver
  features;
- authoritative for policy: it can read the sandbox's *current* declaration per
  request, so an edit changes enforcement for a running sandbox instead of only
  new ones;
- where the compiled `egress.Policy` already lives, with a content-derived
  generation, per-decision `Record`s and engine-endpoint auto-allow rules;
- where request-level (L7) enforcement and credential rewriting belong later.

The industry shape is the same one: a sandbox proxy that answers a disallowed
`CONNECT` with `403` and reloads rules while the sandbox runs, with policy DNS and
transparent TCP capture as later milestones rather than the first one.

## Shape

1. **Listener and dispatch.** The proxy is served on the daemon's existing HTTP
   listener. A request whose target is an absolute URI, or whose method is
   `CONNECT`, is dispatched to the proxy; everything else keeps the current
   handler. No new port and no new topology: the sandbox already reaches this
   listener, which is what `GuestRuntimeBaseURL` names.
2. **Sandbox identity.** The proxy URL carries a per-sandbox credential —
   `http://<sandbox-id>:<token>@<host>:<port>` — minted and revoked with the
   sandbox, the way the runtime LLM facade token already is. A request without a
   valid credential is refused, and the sandbox the credential resolves to is the
   only declaration consulted. The proxy never trusts a caller-supplied project
   or policy.
3. **Decision.** Every connection is decided by `egress.Decide` against the policy
   compiled from that sandbox's current declaration, with the engine's own
   endpoints allowed ahead of the declared entries as engine rules
   (`engine.llm-facade`, `engine.telemetry`, and the proxy's own endpoint). A deny
   answers `403` with the rule id and reason; an allow opens the tunnel to the
   requested host and port.
4. **DNS lives at the proxy.** The proxy resolves the destination, so the guest
   never resolves it and a rebinding answer cannot move the connection: the name
   is what policy approves, the address the proxy dialled is what it reports.
   Pinning the resolved address to declared `allowed_ips` ranges is the schema
   change that follows.
5. **Guest configuration.** The engine adds `HTTP_PROXY`, `HTTPS_PROXY` and
   `NO_PROXY` (engine endpoints excluded) to the sandbox runtime environment, not
   overridable by project env. That is what routes a well-behaved client through
   the decision above.
6. **Honest strength.** With the proxy enabled and reachable, a driver's egress
   strength becomes real (`strength=allowlist`, `mechanism=forward_proxy`) and the
   capability report says so, including the limit that a process which ignores the
   proxy environment bypasses it until transparent capture lands. Where the proxy
   is disabled or unreachable the report stays `strength=none` and the warn/refuse
   behavior above still applies — the engine never claims a limit it does not
   install.

## What this unlocks

- `default: deny` runs *and* filters on the default driver, which removes the
  reason for the refusal path entirely on Docker and makes the execution matrix
  testable without KVM.
- Policy becomes live: because enforcement reads the current declaration per
  request, the daemon-owned policy record and its query/modify RPC become a
  matter of storing what the proxy already reads, not of recreating sandboxes.

## Not in the first slice

Transparent TCP capture and policy DNS (which is what makes a
proxy-environment-ignoring process obey), request-level L7 rules, per-binary
scoping, and credential rewriting at the proxy. Each is a separate step on the
same boundary, and none of them changes the decision path above.

## Delivered, and what it changed

The first slice is implemented and verified on the Docker driver:

- `pkg/agentcompose/proxy` serves the proxy and decides every request;
  `SandboxEgressProxy` (`pkg/agentcompose/adapters`) issues the credential, builds
  the environment, answers the proxy's authentication, and reads the policy;
  `driverRuntimeAdapter.driverSandbox` is the single seam that attaches both the
  environment and the `MediatedByForwardProxy` fact; `HTTP_LISTEN` serves it.
- The engine's own refusals were extended past the address set: the control-plane
  ports `2379`, `2380`, `6443`, `10250`, `10255` are refused under their own rule
  with the role named, and an IPv6-only destination is refused because the
  canonical endpoint identifier cannot express it.
- `allow[].allowed_ips` was added with it, because a name without an address
  constraint is exactly the indirection the proxy would otherwise make
  trustworthy. The constraint travels in the normalized spec, the proto messages
  and the driver-boundary policy, and a mechanism that cannot apply it must now
  say so rather than apply the destinations alone.
- `EGRESS_UNENFORCEABLE_POLICY` defaults to `warn`, so a declaration the platform
  cannot deliver is reported instead of becoming an availability failure.
- **Admission says which mechanism will apply.** `up`/`apply` report, per declaring
  agent, the declaration and the mechanism the creation seam will install
  (`driver.ReportAgentNetworkPolicy`, carried in `ApplyProjectResponse` and
  `ValidateProjectResponse`); `config` prints the declared half locally, because
  it never contacts the daemon. The verdict uses the same mediation predicate the
  seam uses (`SandboxEgressProxy.MediatesDriver`), so a report cannot claim a
  mechanism the creation path would not set, and the daemon fails the request
  rather than describe a project whose policy it cannot compile.

Evidence: `verify-forward-proxy.sh` (9/9) and `verify-allowed-ips.sh` (5/5) on the
Docker driver, plus `verify-warn-default.sh` (13/13) for the fallback.

## Still open

- **Per-sandbox policy record — delivered, with one limit.** A sandbox's runtime
  state records the generation of the effective policy it was created under and
  the mechanism that applied it (`open`, `driver-applied`, `declared-unenforced`,
  `mediated`), and a start that reuses a runtime reports the difference instead
  of assuming the declaration in force reaches it (`egress.RuntimePolicyDriftBetween`).
  The limit: the start path *ensures* a runtime, so it cannot know whether the
  driver built one or started an existing one. `SandboxVMInfo.RuntimeCreated`
  now carries that fact and the Docker driver reports it; a driver that does not
  report it is treated as reuse, which keeps the record conservative (it reports
  a gap rather than claiming enforcement). BoxLite, Microsandbox, and k8s should
  report it too, and the drift report should then be visible per sandbox on an API
  surface rather than only in the daemon log.
- **The engine auto-allow is reported per mechanism, not per agent at
  admission.** `ReportNetworkPolicy`'s engine-endpoint list needs a sandbox's own
  facade address, which does not exist until the sandbox does, so the admission
  report names the exemption through the mechanism's notes and the driver's
  `egress_policy` capability instead of inventing an address. Listing the exact
  endpoints of a *running* sandbox is the remaining step.
