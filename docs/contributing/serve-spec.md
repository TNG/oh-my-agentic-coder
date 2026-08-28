---
title: omac serve — implementation internals
description: Everything on this page is specific to omac serve and its multi-directory architecture.
---

:::note
This page covers the internals of `omac serve` only. For usage instructions, see [advanced/serve-mode.md](../advanced/serve-mode.md).
:::

This page is for contributors who want to understand, extend, or debug `omac serve`.

---

## Key abstractions

### Dir token

When a project directory is activated, omac generates a **random 128-bit dir token** for it. This token appears in every skill URL for that directory — for example, `GET /9f3a1c20/slack/channels` — and in the env vars injected into the sandbox (`OMAC_D_<DIRTOKEN>_<SKILL>_BASE`).

The token is the access control mechanism for multi-directory isolation. Because it is 128-bit random, it cannot be guessed — a session that activates directory A never learns directory B's token and therefore cannot route requests to B's skills. "Bearer capability" means: whoever holds the token can use it; there is no separate authentication check.

The token rotates on each deactivate/reactivate. The server keeps a `byToken` map — the routing table that maps each live dir token to its directory — and looks up every incoming request's token there. When a directory is deactivated, its token is removed from that map, so any later request using the old token gets a 404. A new random token is minted on the next activation. This prevents replay.

### Directory states

A directory moves through these states from the moment it is first requested:

```
unknown ──activate request──▶ activating ──all skills healthy──▶ active
                                  │                                 │
                                  │ skill missing a secret          │ closed or idle too long
                                  ▼                                 ▼
                             active (partial)                  deactivating ─▶ unknown
```

| State              | Meaning |
|--------------------|---|
| `unknown`          | The directory has never been activated in this server session. No routes exist for it. |
| `activating`       | omac is discovering skills, registering them, spawning sidecars, and waiting for health probes. |
| `active`           | All skills are healthy and routed. Normal operating state. |
| `active (partial)` | At least one skill is missing a required secret and cannot start. The other skills work normally. omac returns HTTP 409 with `X-Omac-Reason: pending-credentials` on that skill's routes, telling the caller which secret to supply. |
| `deactivating`     | Sidecars are being stopped (SIGTERM then SIGKILL), routes are being removed. |

`active (partial)` is a deliberate design choice. In serve mode, secrets can be added at any time via `omac secrets set` without restarting the server. Failing the whole directory upfront would force the developer to fix all secrets before any skill works — but in practice, a developer may open a project in Desktop, work with the skills they have, and supply the missing secret later.

### Two scoping keys

There are two distinct identity keys used in different places:

| Key | Value | Lifetime | Used for |
|---|---|---|---|
| **Routing token** | random 128-bit, minted per activation | per activation (rotates on each deactivate/reactivate) | Facade mount paths; controls which dir a caller can reach |
| **Workdir identity** | `sha256(abs(workdir))`, stable across restarts | persistent | Keychain keys; skill registry; skill-dir resolution |

The routing token must be random. The loopback port is reachable by any process running as the same user — including the agent's own npm, pip, or cargo dependencies. A compromised dependency could connect to the control plane and try to call another project's skills. A predictable token, such as a hash of the directory path, could be guessed, giving access to any active directory.

The workdir identity keys a directory's persistent state — its keychain secrets and skill registry. It must be **unique**, so one project's secrets and registrations never collide with another's, and **stable across restarts**, so that state is still found next run instead of being orphaned. Both properties come from the absolute path: the same directory always resolves to the same path, and no two directories share one. A bare name like `acme` would not do — `~/work/acme` and `~/clients/acme` would collide.

### Manifest

When a directory is activated, omac returns a **manifest** — a JSON document listing the directory's workdir-local skills and the server's global skills, each with its state and base URL. Harnesses read it to find each skill's endpoint, and in multi-directory mode to get the namespaced URLs (rather than the flat `OMAC_<SKILL>_BASE` aliases).

```json
{
  "dir": "/Users/me/projects/acme",
  "dir_token": "a17f…d3",
  "state": "active_partial",
  "skills": [
    { "name": "slack", "scope": "workdir", "state": "ready",
      "base": "http://127.0.0.1:51823/a17f…d3/slack" },
    { "name": "email", "scope": "workdir", "state": "pending_credentials",
      "missing": ["EMAIL_API_KEY"],
      "fix": ["omac secrets set email EMAIL_API_KEY"] },
    { "name": "weather", "scope": "global", "state": "ready",
      "base": "http://127.0.0.1:51823/__global__/weather" }
  ]
}
```

### Global vs workdir skills

omac serve has two skill scopes. **Workdir-local skills** belong to a single project and stay isolated from every other; **user-global skills** are shared by all active projects. Routing enforces the split: workdir-local skills sit behind their directory's random [dir token](#dir-token), while global skills sit under one fixed `__global__` namespace.

**Workdir-local skills** (under the project directory) are registered per-project, spawn one sidecar per project, and store credentials under `omac/<workdir-id>/<skill>` in the keychain. Two projects can have a skill with the same name without sharing credentials.

**User-global skills** (under `~/.config/opencode/skills/` or the shared neutral base `~/.config/agents/skills/`) are registered once for the whole server, run as a single shared sidecar, and store credentials under `omac/<skill>`. All active projects share them. This is intentional — a user-global skill is an explicit opt-in to sharing.

Global skills route under a reserved namespace, `__global__`, so they cannot be confused with per-project skills:

```
# Facade route — <mount> is the skill's mount, <rest> its own endpoint path.
/__global__/<mount>/<rest>    # e.g. GET /__global__/weather/forecast

# Base-URL env var injected into the sandbox; the _G_ marks it global.
OMAC_G_<SKILL>_BASE           # e.g. OMAC_G_WEATHER_BASE

# Same URL, unprefixed — kept so skills written for `omac start` still work
# (they call $OMAC_SLACK_BASE directly, without reading the manifest).
# Present only while exactly one directory is active: activating a second
# tears these flat routes down live (deactivating back to one restores them),
# so multi-directory harnesses must read the manifest instead.
OMAC_<SKILL>_BASE
```

`__global__` is a reserved name: the server never assigns it as a directory's token (it is never added to the `byToken` map). This guarantees a `/__global__/…` URL always means global skills and can never collide with a real directory. Without it, a directory that happened to get the token `__global__` would silently receive requests meant for the global skills.

---

## Control-plane API

The control plane is a loopback HTTP server, advertised to the sandbox as `OMAC_CONTROL_BASE`.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/__omac__/activate` | Body `{"dir": "/abs/path"}`. Activates (or returns existing) dir. Response = [manifest](#manifest). Idempotent. |
| `POST` | `/__omac__/deactivate` | Body `{"dir": "/abs/path"}`. Tears down sidecars and routes. |
| `POST` | `/__omac__/reload` | Body `{"dir": "/abs/path"}`. Re-resolves secrets; promotes `pending-credentials` skills to active if their secrets are now present. |
| `GET` | `/__omac__/dirs` | List active dirs and their states. |
| `GET` | `/__omac__/global` | Global skills and their `/__global__/<mount>` URLs. |

---

## Security and isolation

Sidecars run on the host, outside the sandbox, so the agent cannot read their credentials — see [Design → Sidecars run on the host](design.md#sidecars-run-on-the-host-not-inside-the-sandbox) for the general rationale. This section covers the isolation that is specific to serve mode: keeping *multiple directories* apart on one shared server.

Route-level isolation (preventing one project's agent from calling another project's skills) is enforced by the random dir token. Credential isolation keys each workdir-local skill's secrets on its workdir-id (service `omac/<workdir-id>/<skill>`), so two projects' same-named skills get separate keychain entries instead of sharing one. Note this is namespacing, not access control: the workdir-id is a deterministic hash of the path ([Two scoping keys](#two-scoping-keys)), not a secret. It prevents *accidental* sharing — it does **not** stop a malicious sidecar that computes another project's workdir-id and reads its entry, since the OS keychain serves any entry to any process running as the same user. Blocking that requires OS-level confinement (**L3**, deferred).

| Level | Control | Prohibits                                                       | Status |
|---|---|-----------------------------------------------------------------|---|
| Route isolation | Random dir-token routing | One project's agent reaching another's skills                   | shipped |
| L1 — credential isolation | Secrets keyed `omac/<workdir-id>/<skill>` | Same-name sidecars in different projects sharing a credential   | shipped |
| L2 — bundle-hash pinning | Skill source must match what `omac register` approved | A different skill binary swapped in under the same name         | user-global skills only |
| L3 — sidecar OS confinement | Per-sidecar seccomp / Landlock / dedicated uid | A hostile sidecar reading other projects' files or host secrets | deferred |

---

## Milestones

Where the serve-mode implementation stands. ✅ = shipped, ⏳ = not yet.

| Milestone | Status | What it means |
|---|---|---|
| Runtime-mutable routing & sidecars | ✅ | Skill routes and sidecar processes can be added and removed *while the server runs* — the foundation that lets directories be activated and deactivated on the fly. |
| `omac serve` command | ✅ | The long-running server itself: it loads the user-global skills at startup, serves the control-plane API alongside the harness, and can auto-activate one directory (`--workdir`). |
| Auto-register + manifest | ✅ | A directory's skills are registered automatically on first activation; a skill missing a secret returns an actionable error instead of failing silently; activation returns the [manifest](#manifest). |
| OpenCode Desktop integration | ⏳ | Skills activating automatically as you open or switch projects in OpenCode Desktop. omac's side is done; the gap is in OpenCode Desktop, which doesn't yet tell omac when you switch projects. Workaround: restart `omac serve --workdir` at the new project. |
| Hardening & polish | ⏳ | Still to come: shutting down directories left idle too long (to free their sidecars); binding a dir token to the session that created it, so a process that merely learns a token still cannot use it; per-sidecar OS confinement ([L3](#security-and-isolation)); and `omac doctor` support for serve. |
