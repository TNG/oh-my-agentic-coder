---
title: "ADR 0002: External execution through skill sidecars"
description: Keep provider lifecycle and execution connections outside the core.
---

Status: accepted (architecture meeting, 2026-11-24).

## Context

Builds and tests need capabilities unavailable in the agent sandbox, including
dynamic local networking for Gradle and Docker for Testcontainers. We need to
transfer project files, execute commands elsewhere and retrieve results without
weakening host isolation.

## Decision

Use approved skills with host-side sidecars, as proposed in
[variant 3 of the architecture discussion](https://github.com/TNG/oh-my-agentic-coder/pull/417#issuecomment-7127703193).

The existing runtime provides the agent sandbox, sidecar startup, credential
injection and facade. Each skill handles target selection, authorization,
connections, file transfer, execution, results and cancellation.

Lima, microsandbox and a generic Secure Shell (SSH) sidecar are possible
implementations. Provider and transport remain open. Users or providers manage
the environment; an approved skill may also provision and manage it within
user-defined limits.

Keep provider-specific lifecycle and connection handling outside the core.
Do not maintain a parallel direct-access path in the core.

## Rejected alternatives

- [JVM build broker, PR #192](https://github.com/TNG/oh-my-agentic-coder/pull/192):
  adds build-tool, proxy and process-management responsibilities to the core.
- [Managed virtual-machine stack, PR #417 onward](https://github.com/TNG/oh-my-agentic-coder/pull/417):
  couples the core to provisioning and provider lifecycle. A skill can offer
  the same setup convenience.
- External environments with SSH handling in the core: retain a separate
  connection and credential path that the sidecar can own.

## Security requirements

- Keep target credentials and administration interfaces outside the agent
  sandbox and guest. The sidecar verifies the target's identity and authorizes
  each operation against trusted project/session configuration.
- Transfer only sandbox-readable input. Sidecars must not turn caller-supplied
  paths into unrestricted host reads or writes.
- Run repository code in the target environment. Never fall back to host execution.
- Protect host files, services and required network restrictions even against
  guest root. Guest firewalls alone do not suffice. Exclude host Docker sockets,
  credential forwarding, host-execution bridges and uncontrolled mounts or ports.
- Define output, exit status, cancellation and cleanup. Verify isolation for
  each supported provider/runtime/operating-system combination; shared targets
  do not imply isolation between sessions.

## Consequences

The earlier build-broker and managed-machine designs will not proceed as core
features. Their reusable components may move into skills.

Start with the existing skill runtime. Address demonstrated gaps through
separately justified, general runtime changes. A new provider or build tool
does not justify reopening the core-integration approach.

Departures require a superseding architecture decision record explaining the
unmet requirement and why a skill or general runtime extension cannot meet it.
