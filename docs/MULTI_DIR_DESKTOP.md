# Multi-Directory omac for OpenCode Desktop

Status: **draft / spec** — not yet implemented.
Audience: omac maintainers + reviewers.
Related: `oh-my-agentic-coder.md` (master design), `internal/cli/start.go`,
`internal/facade/facade.go`, `internal/supervisor/supervisor.go`,
`internal/skillsource/skillsource.go`.

> **Harness note.** This document was written for the OpenCode harness, but the
> control plane, namespacing, manifest, and isolation design are
> harness-agnostic. The harness is selected by a positional token —
> `omac serve opencode` / `omac serve claude` (default: `opencode`). Each
> harness supplies a **bridge** that calls the same control-plane endpoints
> described here. See [§0 Harness bridges](#0-harness-bridges).

---

## 0. Harness bridges

The "OpenCode-side" responsibilities in this spec (activate on directory open,
surface the manifest to the model, inject per-session skill env, deactivate on
session end) form a single, harness-independent **bridge interface**. omac
ships one bridge per supported harness; both speak only to `OMAC_CONTROL_BASE`
and the `/__omac__/*` endpoints.

| Harness     | Bridge                         | Activation trigger              | Manifest delivery                         | Per-session env                              |
| ----------- | ------------------------------ | ------------------------------- | ----------------------------------------- | -------------------------------------------- |
| OpenCode    | `.opencode/plugins/omac-multidir.ts` | plugin construction + `session.*` events | `experimental.chat.system.transform`      | `shell.env` → `OMAC_D_*` per session (§4.1)  |
| Claude Code | `.claude/` (`settings.json` + `hooks/omac-bridge.sh`) | `SessionStart` hook              | `SessionStart` hook `additionalContext`   | process-level flat aliases (`OMAC_<MOUNT>_BASE`) — see below |
| Pi          | `.pi/extensions/omac-bridge/index.ts` | `session_start` event           | `before_agent_start` event (system prompt)| process-level flat aliases (`OMAC_<MOUNT>_BASE`) |

**OpenCode** has a true per-session env hook, so it can inject distinct
`OMAC_D_<token>_<MOUNT>_BASE` for every session and supports the full
multi-directory isolation described in §8.

**Claude Code** has no per-shell env hook equivalent. Its bridge therefore
relies on the §5.5 single-directory flat aliases that omac already exports into
the inner process environment (`OMAC_<MOUNT>_BASE`, `OMAC_G_<MOUNT>_BASE`),
which Claude Code inherits. This is sufficient for the common
one-directory-per-process case and degrades gracefully when several directories
are active (the manifest still lists every skill's absolute `base` URL, which
the agent can call directly). Claude Code also has no `opencode serve`-style
daemon convention, so under `omac serve claude` the inner command runs as-is
(no subcommand is injected); `omac start claude` is the primary supported mode.

Adding a third harness is: register a descriptor in
`internal/config/harness.go` (name, aliases, inner command, server-launch
convention, bridge directory) and ship a bridge that implements this interface.

**Harness-scoped discovery.** Discovery (workdir and global) is scoped to the
active harness: it scans the harness's own skills dir (`.opencode/skills` /
`.claude/skills`) plus the shared `.agents/skills`, and never the other
harness's dir. In serve mode this scoping applies to per-directory activation
*and* to the cold-start global skills (a global skill registered under another
harness's dir is skipped). A skill name may be registered once per harness; the
manifest a bridge receives therefore only lists skills the active harness can
load. The marketplace `/install` target defaults to the active harness's dir
via the injected `OMAC_HARNESS_SKILLS_DIR`.

---

## 1. Problem

Today omac is a **single-workdir launcher**. `omac start` is anchored to one
`--workdir`, and that workdir decides:

- which skill source roots are scanned (`<workdir>/.agents/skills`,
  `<workdir>/.opencode/skills`) — see `skillsource.Sources`;
- which workdir registry / skill-config layer applies
  (`<workdir>/.opencode/sidecar.json`, `.../skill-config.yaml`);
- the runtime dir (`${TMPDIR}/omac-<sha256(workdir)[:6]>`);
- the facade routes and the `OMAC_*` env injected into the single inner
  command.

The inner command is run **once**, to completion, then everything is torn
down (`start.go` step 8 + deferred shutdowns).

OpenCode Desktop breaks all of those assumptions:

- A Desktop user opens **many projects** (directories) in one running
  backend. The set of directories is **not known up-front** and grows over
  the lifetime of the process.
- Each directory has its **own** `.opencode/skills` / `.agents/skills`, so
  skills are inherently **per-directory**.
- The natural deployment is **`opencode serve`** (one long-lived server)
  that Desktop connects to over a port — not a one-shot `opencode` TUI.

We want: serve OpenCode once, and **lazily bring a directory's skills online
the first time that directory is requested**, without restarting the server
or knowing the directory list in advance.

---

## 2. Decisions (locked for this spec)

These were chosen explicitly; the rest of the spec assumes them.

1. **omac wraps `opencode serve`.** omac remains the outer launcher. The
   profile `inner_cmd` becomes `["opencode", "serve", ...]`. The OpenCode
   server runs *inside* the sandbox; Desktop connects to it over the served
   port. omac keeps owning the sandbox, the facade, and the sidecar
   lifecycle.

2. **One shared sandbox, multiple directories mounted.** There is a single
   nono sandbox and a **single facade** for the whole server process. As new
   directories come online, their skill sidecars are added as **new routes**
   on the existing facade, and (if needed) their paths are mounted into the
   running sandbox. Skills are **namespaced per directory** on the facade so
   two projects can each register a skill called `slack` without colliding.

3. **Auto-register on first request; prompt for secrets later.** When a
   directory is requested for the first time, omac **discovers and
   auto-registers** that directory's skills (no interactive `omac register`
   gate). A skill that needs a required secret/config value still spins up,
   but the facade serves it as **"pending credentials"** until the value is
   supplied out-of-band (Desktop UI / `omac secrets set`). Only that one
   skill is blocked; the rest of the directory comes online normally.

---

## 3. Architecture overview

```
                 host                                   sandbox (one, shared)
  ┌──────────────────────────────┐            ┌───────────────────────────────────┐
  │ omac serve (NEW subcommand)  │            │  opencode serve  (inner_cmd)        │
  │                              │            │     │  HTTP :PORT  ◄───── Desktop    │
  │  control plane:              │            │     │                                │
  │   - dir registry (in-mem)    │            │     ▼  calls OMAC_<dir>_<skill>_BASE │
  │   - lazy activation          │   facade   │  ┌──────────────────────────────┐   │
  │   - facade route table  ─────┼────────────┼─▶│ facade (one) :TCP + bridge.sock│  │
  │   - supervisor (sidecars)    │  routes    │  └──────────────────────────────┘   │
  │                              │            └───────────────────────────────────┘
  │  sidecars (per dir/skill):   │
  │    dirA/slack  → 127.0.0.1:p1│
  │    dirA/email  → 127.0.0.1:p2│
  │    dirB/slack  → 127.0.0.1:p3│
  └──────────────────────────────┘
```

Key shift vs today: the facade and supervisor become **long-lived and
mutable**. They gain "add a route / spawn a sidecar at runtime" and "drop a
route / stop a sidecar" operations, instead of being built once in
`start.go` and frozen.

---

## 4. New concepts

### 4.1 Directory namespace

Each activation of a directory `D` mints a **dir token** — a capability used
to namespace *and authorize* access to that dir's skills:

```
dirtoken = 128-bit crypto-random, minted per activation   // e.g. "a17f…d3"  (recommended)
```

> An earlier draft used `sha256(abs(D))[:8]`. That is **guessable** (the path
> space is small/enumerable), which would let a session address a dir it
> never activated. Use a random per-activation token instead: it is an
> unforgeable bearer capability and it rotates on deactivate/reactivate. The
> server keeps `token → dir` in memory (`byToken`, §7); the path never derives
> the token. See §8.1 for the full rationale.

The token namespaces mounts and env vars so two directories can hold
equally-named skills without collision, *and* gates which dir a caller can
reach.

- **Facade mount:** `<dirtoken>/<skill-mount>` →
  e.g. `GET /9f3a1c20/slack/channels`.
- **Env var into the sandbox:**
  `OMAC_D_<DIRTOKEN>_<SKILL>_BASE` = `http://127.0.0.1:<tcp>/<dirtoken>/<mount>`.
  (Mirrors today's `OMAC_<SKILL>_BASE`; `OmacEnvName` is extended to take a
  dir token prefix — see §7.) When exactly one dir is active, `serve` may also
  emit the unprefixed `OMAC_<SKILL>_BASE` alias for `start`-mode portability —
  see §5.5.
- A per-directory manifest is also exposed (see §6) so OpenCode can resolve
  "which skills exist for the directory I'm working in" without parsing env
  var names.

> **Why namespacing, not per-dir facades:** Decision #2 mandates one shared
> sandbox + one facade. Namespacing keeps a single listener/socket
> (`--open-port` stays a single port) while still isolating directories.

### 4.2 Directory states

```
unknown ──request──▶ activating ──skills healthy──▶ active
                         │                            │
                         │ skill needs secret         │ dir closed / idle
                         ▼                            ▼
                    active(partial)              deactivating ─▶ unknown
```

- **activating:** discovery + auto-register + sidecar spawn + health probe in
  progress.
- **active:** all the directory's registered skills are healthy and routed.
- **active(partial):** directory is usable; ≥1 skill is `pending-credentials`
  (its route returns a structured 409, see §6).
- **deactivating:** idle/closed; sidecars SIGTERM→SIGKILL, routes removed.

### 4.3 Two scoping keys — routing token vs. workdir identity (and versions)

A subtlety that matters once two workdirs can hold the **same-named skill at
different versions** (e.g. A ships `slack@1.0`, B ships `slack@2.0`): there
are **two** distinct keys, used for two different purposes. Don't conflate
them, and in particular **don't scope persistent state by the workdir's bare
name** — `~/work/acme` and `~/clients/acme` are both "acme" and would
collide.

| Key | Value | Lifetime | Used for |
|---|---|---|---|
| **routing token** (§4.1) | random 128-bit | per *activation* (rotates each run) | facade mount + env var; authorizing which dir a caller may reach |
| **workdir identity** | `sha256(abs(workdir))` (stable) | persistent | registry layer, keychain key, skill-dir resolution |

**Why bare name is wrong:** it isn't unique (two paths, same basename) and
isn't even what you want — identity must follow the *directory*, not its
label.

**How this resolves versioning.** Skill identity today is the bare skill
*name* — the registry entry, the keychain service (`omac/<name>`), and the
mount are all keyed by name only; `omac.yaml`'s `version` field is read but
**ignored for identity** (`config.Meta.Version` exists but nothing keys off
it; `keychain.Service` = `"omac/" + name`). So `slack@1.0` and `slack@2.0`
would collapse onto one identity, one set of secrets, one registration. To
let versions coexist per workdir:

1. **Registry is already per-workdir.** Each served dir uses its own
   `D/.opencode/sidecar.json` (§5.2 step 4). Two dirs therefore hold two
   independent entries for `slack`, each with **its own `bundle_hash`** —
   which already differs between `1.0` and `2.0` because the sidecar source
   differs. So per-workdir registries + bundle-hash pinning give you
   version-distinct registrations for free; no `version`-keying needed.

2. **Secrets must become per-(workdir, skill).** This is the part not free
   today. Keychain service changes from `omac/<skill>` to
   `omac/<workdir-id>/<skill>` (workdir-id = `sha256(abs(workdir))`), so B's
   `slack@2.0` token cannot be read by A's `slack@1.0`. Different versions
   plausibly need different credentials/scopes, so this is the strongest
   argument for doing the L1 change (§8.2) in v1. Implemented by threading a
   workdir-id prefix through `keychain.Get/Set` (§7).

3. **Global skills stay shared, single-version, on purpose.** A skill under
   `~/.config/opencode/skills/slack` keeps the unscoped `omac/slack` key and
   is the *same* version everywhere — that is the explicit escape hatch for
   "I want N workdirs to share one skill+credential". Workdir-local =
   scoped/versioned per dir; user-global = shared. This mirrors the existing
   two-layer precedence exactly.

> **Trade-off to accept:** two workdirs that genuinely want the *same* skill,
> *same* version, *same* credentials now each register and each store the
> secret. That is the correct isolation default; the user-global layer is the
> sanctioned way to opt back into sharing.

### 4.4 Global defaults — remember-last-values for fast re-registration

Per-workdir secrets/config (§4.3) gives isolation but reintroduces friction:
the *same* skill registered in many workdirs would re-prompt for the same
values every time. To remove that friction without giving up isolation, omac
keeps a **global "last-known-good" defaults layer** that is *separate* from
the per-workdir values it actually uses at runtime.

**What gets written.** Whenever a secret or config field is saved for a skill
in *any* workdir (via `omac register`, `omac secrets set`, `omac config set`),
omac *also* writes that value to the global defaults, keyed **by skill name
only** (not by workdir, not by version):

- secrets → keychain service `omac/__defaults__/<skill>` (account = secret
  name), parallel to the per-workdir `omac/<workdir-id>/<skill>`;
- config → a `defaults:` block in the global `skill-config.yaml`
  (`~/.config/omac/...`), keyed `<skill>.<field>`.

"Last write wins": registering `slack` in workdir B overwrites the global
default for `slack`, so the defaults always reflect the most recent value the
user supplied anywhere. These defaults are **never read at runtime** — they
are *only* a source of suggested values for future registrations. The values
a sidecar actually receives still come exclusively from the per-workdir store
(§4.3), so updating a default never silently changes a running workdir.

**The `--defaults` flag.** `omac register --defaults <skill>` registers
non-interactively by pulling from the global defaults layer:

```
omac register --defaults slack       # workdir A, first time anywhere:
  → no global default exists for SLACK_TOKEN → PROMPT for it
  → value is saved to A's per-workdir store AND to the global default

omac register --defaults slack       # workdir B, second time:
  → global default exists for SLACK_TOKEN → use it silently, no prompt
  → also copied into B's per-workdir store
```

Precise semantics:

1. For each required secret/config field, resolve a candidate from the
   **global defaults**. If a default **exists**, use it silently. If it does
   **not** exist (truly first time anywhere for that field), **prompt** the
   user even though `--defaults` was given — `--defaults` means "don't ask me
   for things I've already answered", not "skip required values I've never
   set".
2. Every value used (whether taken from a default or freshly prompted) is
   written to **both** the per-workdir store (the runtime source of truth)
   **and** refreshed into the global defaults (so the newest value wins).
3. Optional fields with no default and no prompt answer stay unset, exactly
   as today (recorded in `SkippedSecretNames`/`SkippedConfigFields`).
4. Without `--defaults`, behaviour is unchanged: interactive prompts for
   everything not already in the per-workdir store.

This makes the common "I use `slack` in lots of projects" path a single
non-interactive command from the second workdir onward, while the first
registration anywhere still safely collects the values once. Isolation is
preserved: the *defaults* are global, but each workdir still owns its own
copy and a leaked/rotated default doesn't reach into other workdirs until
they re-register.

> **Security note.** The global defaults keychain entry
> (`omac/__defaults__/<skill>`) holds a real credential and is as sensitive as
> any per-workdir secret; it inherits the same keychain protection. A user who
> wants strict per-workdir credentials (different token per project) simply
> doesn't use `--defaults` and answers the prompts per workdir. `omac
> deregister --purge-secrets` should offer a `--purge-defaults` companion to
> wipe the global default too.

> **Do not confuse "global defaults" with "global skills" (§4.5).** A *global
> default* is a remembered value for a workdir-*local* skill, used to pre-fill
> future registrations. A *global skill* is a skill whose *source directory*
> lives under `~/.config/{opencode,agents}/skills` and is shared, single-copy,
> across all workdirs. They are orthogonal: for a global skill, `--defaults`
> is a no-op because its values are already shared by definition.

### 4.5 Global (shared) skills in serve mode

omac already has a **two-layer skill model** (see `skillsource` and
`register.go:101` `global := src.Kind == "user-global"`): a skill resolved
from a workdir-local root is per-workdir; a skill resolved from a user-global
root (`~/.config/{opencode,agents}/skills`, plus the legacy/XDG variants) is
**registered once and shared by every workdir**, with its registry entry and
config in the global stores and its secret under the unscoped `omac/<skill>`
key. The multi-dir/serve concept must preserve this layer rather than
flattening every discovered skill into a per-dir one. Three rules make that
explicit.

**Rule 1 — a global skill is the server's skill, not a dir's.** It is
discovered by `skillsource.Discover(D)` for *every* dir (the global roots are
always scanned), but it **belongs to the server**, not to `D`. It is
registered/activated **once** (cold start, or first time it is seen), into the
**global** registry/keychain/config — never per-dir. It is therefore exempt
from:

- the per-`(workdir, skill)` keychain keying of §4.3 (keeps `omac/<skill>`);
- the bundle-hash-per-dir versioning of §4.3 (one global copy ⇒ one version
  everywhere — the documented escape hatch for "share one skill across
  projects");
- the `--defaults` mirroring of §4.4 (its values are already global).

**Rule 2 — one shared sidecar, not one per dir.** A global skill spawns
**exactly one** sidecar for the whole server (matching single-process today),
not one per active directory. Per-dir activation does **not** re-spawn or
re-register it; it only *exposes* the already-running global skill to that
dir (Rule 3). This avoids the "N projects open ⇒ N identical `slack`
sidecars" blow-up the earlier draft would have caused.

**Rule 3 — reachable from every active dir, via a reserved namespace.**
Workdir-local skills route under the per-activation dir token
(`/<dirtoken>/<mount>`, §4.1). Global skills get a **stable reserved
namespace** instead:

```
/__global__/<mount>/<rest>          facade route for a global skill
OMAC_G_<SKILL>_BASE                 env var (note the G_, parallel to the D_ form)
OMAC_<SKILL>_BASE                   flat alias (same URL) for start-mode skills
```

The `__global__` segment is a reserved, non-mintable token (a dir can never
be assigned it), so there is no collision with dir tokens and no ambiguity
about which upstream a request targets.

**Flat alias for compatibility.** A global skill *also* gets the unprefixed
`OMAC_<MOUNT>_BASE` env var (pointing at the same `/__global__/<mount>` URL),
because skills authored for single-workdir `start` mode hardcode that flat
name in their `SKILL.md` (e.g. `skill-marketplace` reads
`OMAC_SKILL_MARKETPLACE_BASE`). A global skill's mount is unique server-wide
(it lives under the reserved `__global__` namespace), so the flat alias is
unambiguous — unlike per-dir workdir-local skills, where flat names would
collide across directories (hence §5.5 only emits the flat alias when exactly
one dir is active). For global skills the flat alias is always safe and is
emitted unconditionally. Every active dir's **manifest** (§6.3)
lists the server's global skills alongside that dir's local ones, each marked
`"scope": "global"`, so the agent in any project sees them with their
`OMAC_G_*` URLs. (Alternative considered: alias the global skill under every
dir token. Rejected — it multiplies routes, muddies access logs, and gives no
isolation benefit since global skills are shared by design. A single reserved
namespace is simpler and honest about the sharing.)

**Interaction with secret isolation (§8).** Because a global skill is shared,
*every* served project's agent can invoke it with the *same* credential. That
is the intended semantics of opting a skill into the global layer, but it is
also strictly weaker than per-workdir isolation — so it must be a deliberate
user choice (placing the skill under `~/.config/.../skills`), never the
default. Workdir-local remains the default and the isolated path; global is
the opt-in "shared utility" path. Activation policy (§8.4) still applies: a
global skill is only activated if the server's policy allows it, and
`install_scripts` are still never executed.

---

## 5. Control plane: `omac serve`

A new subcommand `omac serve` (sibling of `start` in
`internal/cli/cli.go`). It reuses `start`'s phases 1, 4–8 but makes the
facade/supervisor long-lived and adds an **activation loop**.

### 5.1 Startup (cold)

1. Load launcher config + pick profile (same as `start` steps 1–2 setup).
2. Create the runtime dir, but keyed on a **server identity** instead of a
   single workdir: `${TMPDIR}/omac-serve-<sha256(server-root)[:6]>`.
3. Start the facade with an **empty route table** (today it requires routes
   up front; relax that — empty is legal).
4. Start the supervisor with **no per-dir sidecars**.
5. **Activate user-global skills once (§4.5).** Load the global registry
   (`registry.LoadGlobal`), resolve each entry's secrets/config from the
   global stores, and — for the ready ones — spawn **one** sidecar each and
   mount it under the reserved `/__global__/<mount>` namespace. Global skills
   that are `pending-credentials`/`broken` get the same stub-route treatment
   as in §5.2, just under `__global__`. This is the only activation that
   happens before any directory is requested.
6. Build the inner argv as `opencode serve` and `sandbox.Exec` it — but in
   `serve` mode omac does **not** block on the inner process to do route
   mutation; the activation loop runs concurrently (see §5.3 for the
   exec/lifecycle change required).
7. Inject the global env (`OMAC_SOCKET`, `OMAC_HOST`, `OMAC_PORT`,
   `OMAC_BASE`, `OMAC_VERSION`), the **`OMAC_G_<SKILL>_BASE`** vars for the
   global skills mounted in step 5, plus the **control-plane URL** (§6) so
   OpenCode can ask omac to activate directories.

At cold start, no *directory* is active and `OMAC_SKILLS` lists only the
global skills (the shared baseline available to every project). Per-dir
skills are added lazily (§5.2).

### 5.2 Lazy activation (the core new behavior)

Trigger: a directory `D` is requested for the first time. Two possible
trigger sources (both supported; see §6):

- **Pull:** OpenCode (or Desktop) calls the control-plane endpoint
  `POST /__omac__/activate {dir: "/abs/path"}` when it opens a project /
  starts a session for `D`.
- **Push fallback:** if integration via OpenCode is not wired yet, omac can
  watch a hint file or accept directories from a config list (see §9
  "phasing").

Activation steps for `D`:

1. **Guard / dedupe.** If `D` is already `active`/`activating`, return its
   current manifest. Concurrent requests for the same `D` coalesce on a
   per-dir mutex.
2. **Validate** `D` is a real directory and (policy, §8) is allowed.
3. **Discover and split by layer.** `skillsource.Discover(D)` walks
   `D/.agents/skills`, `D/.opencode/skills`, and the user-global roots with
   workdir-wins precedence. Keep each result's `src.Kind` and partition:
   - **workdir-local** (`src.Kind == "workdir"`) → belongs to `D`; proceed
     with steps 4–7 below.
   - **user-global** (`src.Kind == "user-global"`) → belongs to the *server*,
     **already activated at cold start under `/__global__/`** (§4.5, §5.1).
     Do **not** re-register, re-resolve secrets, or re-spawn it. The only
     per-dir action is to **reference** it in `D`'s manifest (step 9) with
     `"scope": "global"`. If a global skill was seen for the first time only
     now (e.g. the user dropped it into `~/.config/.../skills` after cold
     start), activate it once into the global layer here, then treat it as
     global from then on.
4. **Auto-register the workdir-local skills only** (Decision #3). For each
   local skill not already in the **workdir registry of `D`**, write a
   registry entry + record bundle hash. This is `omac register` minus the
   interactive prompting. Reuse `registry.WithLock(D, …)` to stay race-safe
   with any concurrent CLI `omac register`. (Global skills are never written
   to `D`'s registry — that is the whole point of the global layer.)
5. **Resolve secrets/config (workdir-local skills only)** exactly like
   `start.go` step 3, but from the **per-(workdir, skill)** keychain key and
   `D`'s config store (§4.3). Bucket each into:
   - **ready:** all required secrets/config present → spawn.
   - **pending-credentials:** ≥1 required value missing → still create the
     route, but back it with a stub handler returning 409
     `X-Omac-Reason: pending-credentials` and a JSON body listing the missing
     `omac secrets set …` / `omac config set …` commands. Do **not** spawn the
     sidecar yet.
   - **broken:** `omac.yaml` invalid / bundle drift without
     `--accept-skill-changes` → route returns 502
     `X-Omac-Reason: skill-broken` with diagnostics. (Bundle-drift policy for
     serve mode is configurable; default = refuse the single skill, don't
     fail the whole dir.)
6. **Spawn** ready workdir-local sidecars via the long-lived supervisor
   (`AddSidecar`, §7), health-probe them. (Global skills are already running
   from cold start — not spawned here.)
7. **Mount workdir-local routes** on the long-lived facade under
   `<dirtoken>/<mount>` (`AddRoute`, §7). Global skills keep their existing
   `/__global__/<mount>` routes — nothing per-dir is mounted for them.
8. **Mount path into sandbox** if the profile sandbox doesn't already grant
   `D` filesystem visibility (see §5.4 — this is the trickiest part).
9. Mark `D` `active` (or `active(partial)`), build its manifest as the
   **union** of `D`'s workdir-local skills (`"scope": "workdir"`) and the
   server's global skills (`"scope": "global"`, §4.5), publish it (§6), and
   return it to the caller.

Later, when a pending-credentials skill gets its secret (via
`omac secrets set` or a Desktop call to `POST /__omac__/reload` for the dir),
omac spawns the now-ready sidecar and swaps the stub route for a live route —
no server restart.

### 5.3 Inner-process lifecycle change

`sandbox.Exec` today *blocks* until the inner command exits, and route
construction all happens before exec. For `serve` mode we need the activation
loop to mutate facade/supervisor **while `opencode serve` is running**.

Required change: split exec so omac (the parent) keeps a control goroutine
alive alongside the child:

- Run `opencode serve` as the child (still in its own process group, same
  signal-forwarding contract as `sandbox.Exec`).
- The activation control plane (an HTTP server on a **host-side** loopback
  port or Unix socket — distinct from the facade) runs in the omac parent.
- On child exit (server shut down), tear down all sidecars + facade (the
  existing deferred-cleanup contract, generalized to "all dirs").

**Implemented** as `sandbox.ExecWithReady(argv, env, onReady)`: it is
`sandbox.Exec` factored to invoke `onReady` on a goroutine immediately after
the child starts (and the terminal is handed over), then blocks on the child
exactly as before — preserving the signal/tty contract. `omac serve` starts
the facade + control-plane HTTP server first, then calls `ExecWithReady` with
`opencode serve` as the inner command; the control plane mutates the facade /
supervisor while the child runs, and the deferred `facade.Close()` +
`supervisor.ShutdownAll()` tear everything down on child exit. `--no-inner`
runs the control plane alone (for headless/testing drivers).

### 5.4 Sandbox filesystem visibility — open design question

One shared sandbox must be able to **read the files** of every directory it
serves. There are two sub-cases:

- **Skills:** the sidecars run on the **host** (omac parent), outside the
  sandbox — so the sandbox does not need filesystem access to the skill dirs
  for the sidecars to work. Good.
- **OpenCode editing the project files:** `opencode serve` runs *inside* the
  sandbox and must read/write the project's source. With nono, the sandbox's
  filesystem allow-list is fixed at launch.

Options (decide before implementation):

| Option | Idea | Cost |
|---|---|---|
| **A. Broad root** | Mount a common parent (e.g. `$HOME/projects`) read/write at launch; all served dirs live under it. | Simple; weaker isolation; requires users keep projects under one root. |
| **B. Pre-declared roots** | `omac serve --root <dir> [--root <dir> …]`; only those subtrees are mountable; activating a dir outside any root is rejected. | Predictable; still needs the list up-front (partially conflicts with "unknown up-front", but at the *root* granularity, not project granularity). |
| **C. Dynamic remount** | Re-launch / extend the sandbox's allow-list when a new dir is activated. | Matches the lazy ideal; **may be impossible with nono** without restarting the sandbox — needs a spike. |

**Recommendation for v1:** Option **B** (pre-declare a small set of roots;
projects under them activate lazily). It satisfies the real requirement —
*individual project directories are unknown up front* — while keeping nono's
static allow-list workable. Revisit C if/when nono supports live remounts.

### 5.5 Single-directory / `omac start` compatibility

The multi-dir concept must not regress the common case: **one developer,
`opencode` in one project.** Two guarantees and one convenience handle this.

**Guarantee 1 — `omac start` is untouched.** Everything in this spec is
*additive*: `serve` is a new subcommand sibling to `start` (§5); §7 only adds
`serve.go` and *new* facade/supervisor methods (`AddRoute`, `AddSidecar`),
leaving `StartAll`/`ShutdownAll` and the whole `start.go` flow intact. So the
single-directory workflow is exactly as today:

- `omac start` wraps the **TUI** (`opencode`), one `--workdir`, exec-and-wait,
  teardown on exit;
- flat mounts `/<mount>` and unprefixed `OMAC_<SKILL>_BASE`;
- per-workdir registry + the existing (name-keyed) secrets.

A user who never touches Desktop never touches `serve`. The two L1/§4.3 secret
changes are scoped to `serve`/multi-dir activation paths; **single-dir `start`
keeps its current name-keyed secret behavior** unless and until it is
explicitly migrated (call this out in review — see §10 Q9).

> `start` = TUI in one directory. `serve` = long-lived server for one *or
> many* directories (the Desktop path). They coexist; neither replaces the
> other.

**Guarantee 2 — `serve` degenerates cleanly to one directory.** Running
`serve` with exactly one active dir is well-behaved: cold start brings up
global skills under `/__global__/`, the single `activate` mounts that dir's
skills under its token, and the manifest is that dir's local ∪ global skills.
Nothing about the design *requires* more than one directory.

**Convenience — `omac serve --workdir <dir>` (auto-activate one dir).** To
make the single-dir-with-serve case ergonomic (no external `activate` call
needed, closing the §10 chicken-and-egg where a lone dir never activates
without a Desktop hook), `serve` accepts `--workdir`:

- at cold start, omac **pre-activates** exactly that directory (same path as a
  `POST /__omac__/activate`), so skills are live the moment `opencode serve`
  starts;
- further `activate` calls (from Desktop) still work and add *more* dirs — so
  `--workdir` is a seeded starting point, not a cap.

**Env-var portability across modes (decision needed — §10 Q10).** Under
`serve`, a dir's skill is `OMAC_D_<token>_SLACK_BASE` at `/<token>/slack`;
under `start` it is `OMAC_SLACK_BASE` at `/slack`. A `SKILL.md` that hardcodes
`OMAC_SLACK_BASE` therefore works under `start` but breaks under `serve`.
Recommended fix: **when (and only when) exactly one directory is active**,
`serve` also emits the unprefixed `OMAC_<SKILL>_BASE` aliases (pointing at the
same upstream as the tokenized form). This makes a skill behave identically in
both modes for the single-dir case, while the tokenized form remains the only
one available once a second directory is activated (because flat names would
then be ambiguous — exactly the collision §4.1 namespacing exists to prevent).
Skills intended to be Desktop/multi-dir-aware should read the manifest (§6.3)
rather than guess env-var names.

---

## 6. Control-plane & data-plane API

### 6.1 Control plane (omac parent ⇄ OpenCode/Desktop)

Exposed by omac on a dedicated loopback port/socket, advertised to the
sandbox as `OMAC_CONTROL_BASE`.

- `POST /__omac__/activate` — body `{ "dir": "/abs/path" }`.
  Activates (or returns existing) directory. Response = the dir manifest
  (§6.3). Idempotent.
- `POST /__omac__/deactivate` — body `{ "dir": "/abs/path" }`. Tears down a
  dir's sidecars/routes.
- `POST /__omac__/reload` — body `{ "dir": "/abs/path" }`. Re-resolves
  secrets/config and promotes any `pending-credentials` skills.
- `GET /__omac__/dirs` — list active dirs + states.
- `GET /__omac__/dirs/{dirtoken}/manifest` — the manifest (§6.3).
- `GET /__omac__/global` — the server's global (shared) skills and their
  `/__global__/<mount>` URLs (§4.5). Available to every session; included in
  each dir manifest too, so a dedicated call is rarely needed.

### 6.2 Data plane (sandbox → skill sidecars)

Unchanged transport (facade reverse proxy), only the path is namespaced:

```
workdir-local skill:
  http://127.0.0.1:<tcp>/<dirtoken>/<mount>/<rest>     (preferred, TCP)
  http+unix://<bridge.sock>/<dirtoken>/<mount>/<rest>   (fallback)

global (shared) skill (§4.5):
  http://127.0.0.1:<tcp>/__global__/<mount>/<rest>
  http+unix://<bridge.sock>/__global__/<mount>/<rest>
```

`X-Forwarded-Prefix` becomes `/<dirtoken>/<mount>` or `/__global__/<mount>`.
`facade.splitMount` must learn to split a **two-segment** prefix when in serve
mode, where the first segment is either a live dir token or the reserved,
non-mintable literal `__global__` (see §7).

### 6.3 Per-directory manifest

So OpenCode can map "the directory I'm in" → "the skills + URLs I can call":

The manifest is the **union** of the directory's own workdir-local skills and
the server's global skills (§4.5). Each entry carries a `scope` so the agent
knows whether a skill is isolated to this project or shared:

```json
{
  "dir": "/Users/me/projects/acme",
  "dir_token": "a17f…d3",
  "state": "active_partial",
  "skills": [
    { "name": "slack", "scope": "workdir", "mount": "slack", "state": "ready",
      "base": "http://127.0.0.1:51823/a17f…d3/slack",
      "socket_base": "http+unix://%2F.../a17f…d3/slack" },
    { "name": "email", "scope": "workdir", "mount": "email",
      "state": "pending_credentials",
      "missing": ["EMAIL_API_KEY"],
      "fix": ["omac secrets set email EMAIL_API_KEY"] },
    { "name": "weather", "scope": "global", "mount": "weather", "state": "ready",
      "base": "http://127.0.0.1:51823/__global__/weather",
      "socket_base": "http+unix://%2F.../__global__/weather" }
  ]
}
```

- `scope: "workdir"` — isolated to this directory; per-`(workdir, skill)`
  secret (§4.3); routed under the dir token.
- `scope: "global"` — the shared server skill (§4.5); one sidecar for the
  whole server; unscoped `omac/<skill>` secret; routed under `__global__`.
  Identical across every dir manifest.

OpenCode can fetch this when it opens a project and inject the appropriate
`OMAC_*` knowledge into the skill `SKILL.md` activation context. (How
OpenCode surfaces this to the agent is OpenCode-side and out of scope here;
omac just needs to publish it.)

---

## 7. Required code changes (by package)

| Package / file | Change |
|---|---|
| `internal/cli/cli.go` | Register new `serve` subcommand. |
| `internal/cli/serve.go` (new) | Cold start, activation loop, control-plane HTTP server, generalized teardown. Reuses helpers from `start.go` (`createRuntimeDir`, secret/config resolution, `findUnregisteredSkills` logic). `--workdir <dir>` (§5.5) pre-activates one directory at cold start so single-dir use needs no external `activate` call. |
| `internal/facade/facade.go` | (a) Allow empty initial route table. (b) `AddRoute(Route)` / `RemoveRoute(mount)` under the existing `mu`. (c) Two-segment mount splitting (`<dirtoken>/<mount>` **or** `__global__/<mount>`, §4.5) — extend `splitMount`; reserve `__global__` as a non-mintable token. (d) Stub-route support for `pending-credentials` (409) and `skill-broken` (502). |
| `internal/supervisor/supervisor.go` | `AddSidecar(ctx, SidecarSpec) (Running, error)` and `StopSidecar(name)` for runtime mutation; keep `ShutdownAll` for teardown. Today only `StartAll`/`ShutdownAll` exist. Global skills (§4.5) spawn one sidecar each at cold start. |
| `internal/sandbox/launcher.go` | Generalize `OmacEnvName`/`OmacEnvValue`/`OmacTCPEnvValue` to take a prefix: dir-token → `OMAC_D_<TOKEN>_<SKILL>_BASE` (workdir-local) and the reserved global form → `OMAC_G_<SKILL>_BASE` routing to `/__global__/<mount>` (§4.5). **Single-dir alias (§5.5):** when exactly one dir is active, also emit unprefixed `OMAC_<SKILL>_BASE` so skills are portable between `start` and `serve`; drop the aliases as soon as a 2nd dir activates. Split `Exec` so serve mode can run the child concurrently with the control plane (or add `ExecAsync`). Decide nono mount strategy (§5.4). |
| `internal/registry` | No schema change. Add an internal "auto-register" helper that writes an entry without prompting (reuse `WithLock`). Each served dir uses **its own** workdir registry (`D/.opencode/sidecar.json`) for its workdir-local skills; **global skills live only in the global registry** (`registry.LoadGlobal`) and are activated once at cold start (§4.5, §5.1) — never written per-dir. |
| `internal/keychain` | **L1 isolation:** key secrets by `(workdir, skill)` — service `omac/<workdir-id>/<skill>`, where `workdir-id = sha256(abs(workdir))` (the **persistent** identity, NOT the per-activation routing token; see §4.3) — so same-named skills (incl. different *versions*) in different dirs don't share a credential. Thread an optional workdir-id prefix through `Get`/`Set`; legacy `omac/<skill>` entries remain for **user-global** skills, which are shared on purpose. **Global defaults (§4.4):** add a `__defaults__` pseudo-workdir-id (service `omac/__defaults__/<skill>`); every secret write mirrors into it, and `--defaults` reads from it. |
| `internal/skillconfig` | **Global defaults (§4.4):** add a `defaults:` block (keyed `<skill>.<field>`) to the global `skill-config.yaml`; every config write mirrors into it; `--defaults` reads from it. Runtime resolution still uses only the per-workdir store. |
| `internal/cli/register.go` | Add `--defaults` flag (§4.4): for each required field, use the global default if present (silent), else prompt even under `--defaults`; write every resolved value to both the per-workdir store and the global defaults. Add `--purge-defaults` to `omac deregister`. |
| `internal/config/launcher.go` | New `serve`-specific knobs: `Serve.Roots []string`, `Serve.IdleDirTimeoutSecs`, `Serve.AutoRegister bool`, `Serve.BundleDriftPolicy` (`refuse`|`accept`), `Serve.RequireApproval bool`, `Serve.SessionTokenBinding bool` (§8.1.3). |
| docs | This file; plus a `serve` section in `oh-my-agentic-coder.md` and a note in `README.md`. |

State held by `omac serve` (in-memory, parent process):

```go
type skillRoute struct {
    Name  string                         // skill name
    Mount string                         // facade mount segment
    State string                         // ready|pending_credentials|broken
    // running sidecar handle, etc.
}
type dirState struct {
    Dir    string
    Token  string                        // random 128-bit per-activation capability (§8.1.1)
    State  string                        // activating|active|active_partial|deactivating
    Skills map[string]*skillRoute        // workdir-LOCAL skills only; mount -> handle
    mu     sync.Mutex
}
type server struct {
    facade  *facade.Facade
    sup     *supervisor.Supervisor
    dirs    map[string]*dirState         // abs dir -> state (workdir-local skills)
    byToken map[string]*dirState         // token -> dir (path never derives the token)
    global  map[string]*skillRoute       // §4.5 shared skills; mount -> handle,
                                          // routed under /__global__/, spawned once at cold start
    mu      sync.RWMutex
}
```

A dir's published manifest (§6.3) is `dirState.Skills` ∪ `server.global`; the
`global` map is owned by the server, never duplicated per dir.

---

## 8. Cross-directory isolation in a single process

> **The central question:** with one omac process serving many directories,
> can we guarantee that a request for a skill cannot be run against an
> *unauthorized* other work directory?

Short answer: **the routing/request surface can be fully locked down in a
single process; the sidecar-process surface (shared secrets, host filesystem)
cannot be — those need either a data-model change or OS-level confinement.**
Splitting the question into its two distinct surfaces is essential because
they have opposite answers.

### 8.0 "Can workdir A use a skill that belongs to workdir B?"

The most concrete form of the question. Answer depends on *which* kind of
skill, and on the routing scheme we pick:

| What A tries to use | Discovered by A? | Routable by A? | Verdict |
|---|---|---|---|
| B's **workdir-local** skill (`B/.opencode/skills/…`) | No — `skillsource.Discover(A)` only scans A's roots + global; it never reads `B/`. A's manifest (§6.3) doesn't even list it. | Only if A can name B's route (see token scheme below). | **No** (with random tokens) |
| A **user-global** skill (`~/.config/opencode/skills/…`) | Yes — global roots are scanned for every workdir, on purpose. | Yes — via the reserved `/__global__/<mount>` route (§4.5), reachable from every dir, one shared sidecar. | **Yes, by design** |
| A skill *named* the same in both A and B (both workdir-local) | Each is discovered only in its own workdir; they are **separate** sidecars/routes (`/<A-token>/slack` vs `/<B-token>/slack`). | Each session reaches only its own. | **Separate** (but see secret note ↓) |

> **Global skills are the *only* intentional cross-workdir path (§4.5).** A
> user-global skill is shared on purpose: every active dir sees it in its
> manifest and can call it under `/__global__/<mount>` with the *same*
> credential. This is strictly weaker than per-workdir isolation, so it is
> opt-in by where the skill *lives* (`~/.config/.../skills`), never the
> default. A workdir-local skill is never reachable from another workdir.

The "routable by A?" column is decided entirely by the mount/token scheme:

- **Flat mounts** (`/slack/…`, today's single-workdir scheme): **A *can*
  reach B's skill.** One shared facade + one `/slack` route means whoever
  owns that mount serves every session. **Unacceptable for multi-dir.**
- **Guessable token** (`sha256(dir)[:8]`): A can enumerate/derive B's path
  and call `/<B-token>/slack/…`. **Still leaks.**
- **Random per-activation token + empty-default route table** (§8.1,
  recommended): B's token is a 128-bit secret A never receives, and B's route
  doesn't exist until B activates. **A cannot name or reach B's skill.**

> **The trap even when routing is perfect:** if A and B each have their *own*
> `slack` skill, routing keeps the *requests* separate — but today both
> sidecars read the **same** `omac/slack` keychain secret (keyed by skill
> name, `start.go:267`). That is not "A using B's skill", but it *is* A and B
> wielding the **same credential/authority**. If the intent of the question is
> "can A act with B's authority", this is the path that survives perfect
> routing isolation. Fix = key secrets by `(workdir, skill)` (§4.3, §8.2 L1),
> slated for v1. The same fix also lets different *versions* of a same-named
> skill carry different credentials.

### 8.1 Surface 1 — request routing (FULLY enforceable in-process)

The facade is a pure in-process Go reverse proxy. It forwards a request only
if the request path resolves to a mount present in `f.routes`; everything
else returns `404 unknown-mount` (`facade.go:230`). Three layered controls
make cross-dir routing **unforgeable**:

1. **Per-dir capability token.** Mounts are namespaced `/<dirtoken>/<mount>`
   where `dirtoken` is a **random 128-bit value minted per activation** (see
   §4.1). A caller can only reach dir B's sidecar if it presents B's token —
   which it never learns unless it itself activated B. The token is an
   unforgeable bearer capability, not just a label: it is *not* derived from
   the path (a `sha256(dir)` token would be guessable, since the path space is
   small/enumerable), and it rotates on deactivate/reactivate. The server
   keeps `token → dir` in memory (`byToken`, §7); the path never derives it.

2. **Mount table is the allow-list.** A dir that was never activated has
   **zero** routes, so there is no path that reaches it — "unauthorized dir"
   is the default state, not an error path. Activation (and thus route
   creation) is gated by policy (§5.4 Option B roots + optional approval).

3. **Session→dir binding (defense in depth).** Optionally require an
   `X-Omac-Dir-Token` header *in addition to* the path token, and have the
   control plane hand each OpenCode session exactly the token(s) for the dirs
   that session is allowed to use. Then even a path-token leak between
   sessions is insufficient. The facade rejects a mismatch with
   `403 X-Omac-Reason: dir-token-mismatch`. This requires the facade to learn
   the binding at `AddRoute` time (extend `Route` with an allowed-token set).

With (1)+(2) you already get: **a session cannot route a request to a
directory it did not activate, and cannot reach an un-activated directory at
all.** That is the affirmative answer to your question *for the request
surface*, achievable with a single process and no OS help.

### 8.2 Surface 2 — the sidecar process (NOT isolated by routing alone)

This is the surface that single-process + namespacing does **not** protect,
and it must be stated plainly:

- **Sidecars run on the host, outside the sandbox**, as ordinary child
  processes of omac (`supervisor.go` `cmd.Start`). They inherit the omac
  user's filesystem rights. `OMAC_WORKDIR` (`supervisor.go:165`) is
  *advisory* — nothing stops a sidecar's code from reading another served
  project's files, or `~/.ssh`, etc. Routing isolation says nothing about
  what a sidecar *does once it's running*.

- **Secrets are keyed by skill name, so same-named skills share a credential**
  (`start.go:267` `keychain.Get(e.Name, …)`, keychain service `omac/<skill>`).
  If dir A and dir B both ship a `slack` skill, they resolve the **same**
  `omac/slack` secret. Namespacing the *route* does not separate the
  *credential*. A malicious `dirB/.opencode/skills/slack` would receive dir
  A's Slack token simply by being named `slack`.

Mitigations, in increasing strength:

| Level | Control | Stops |
|---|---|---|
| L0 (today) | none beyond routing | nothing on this surface |
| L1 | **Key secrets by `(workdir, skill)`** — keychain service `omac/<workdir-id>/<skill>` (`workdir-id = sha256(abs(workdir))`, persistent — §4.3) | same-name credential sharing across dirs, incl. across skill *versions* |
| L2 | **Per-dir skill identity pinning** — bundle-hash + source-root must match what was approved for *that* dir; reject a skill whose code differs from the one the user vetted (the bundle hash also distinguishes versions) | a swapped/tampered same-named skill |
| L3 | **Per-sidecar OS confinement** — launch each sidecar under its own `sandbox-exec`/seccomp/landlock profile (or a dedicated uid) scoped to its own skill dir | sidecar reading other dirs' files / host secrets |

L1 is a contained change (thread a key prefix through `keychain.Get/Set` and
the resolution in `start.go`/`serve.go`) and **should be in v1** — it closes
the most surprising hole (silent credential sharing). L2 is moderate. **L3 is
the only thing that truly contains a hostile sidecar, and it cannot be done
by the facade or by being single-process** — it is OS-level work, deferred.

> **Global skills are exempt from L1 by design (§4.5).** A user-global skill
> *intends* to share one credential across all workdirs, so it keeps the
> unscoped `omac/<skill>` key; L1's `(workdir, skill)` keying applies only to
> workdir-local skills. The carve-out is safe because a global skill is a
> single shared sidecar the user explicitly opted into — not a per-dir
> skill that could be shadowed by a same-named impostor in another project.

### 8.3 Honest threat-model summary

| Threat | Single-process mitigation | Status |
|---|---|---|
| Session routes to a dir it never activated | random per-activation token + empty default route table | ✅ enforceable |
| Session forges another session's dir access | session→dir token binding (§8.1.3) | ✅ enforceable (opt-in v1) |
| Reaching an un-approved dir at all | Option B roots + activation policy | ✅ enforceable |
| Same-named skills (or different versions) silently share a secret | key secrets by `(workdir, skill)` (§4.3, L1) | ⚠️ needs data-model change (v1) |
| Tampered same-name skill swapped in | per-dir bundle-hash pin (L2) | ⚠️ moderate work |
| Hostile sidecar reads other dirs' files | per-sidecar OS sandbox (L3) | ❌ not single-process; OS-level, deferred |

So: **yes** for the request surface (and that is exactly the "can a request
for skill X be run against unauthorized dir Y" question — the answer is no, it
cannot, once tokens + the empty-default-route model are in place). **Not by
routing alone** for credential and filesystem isolation — those require L1
(do it in v1) and L3 (OS confinement, future work).

### 8.4 Other trust notes

- **Auto-register is a trust change.** Decision #3 makes activation
  auto-register. Mitigations: only dirs under `Serve.Roots` activate;
  `install_scripts` are **still never executed** (unchanged invariant);
  bundle-drift policy defaults to `refuse` per-skill; optional
  `Serve.RequireApproval` restores a TOFU confirmation via the control plane.
- **`opencode serve` itself** runs inside the one shared sandbox and can read
  every mounted project root (Surface 2 applies to it too). Option B's narrow
  roots limit blast radius; this is weaker than today's one-workdir isolation
  and is an explicit, documented consequence of Decision #2.

---

## 9. Phasing / milestones

1. **M1 — Mutable facade + supervisor. ✅ done.** `AddRoute`/`RemoveRoute`/
   `HasRoute`/`UpstreamPort`, `AddSidecar`/`StopSidecar`, empty initial route
   table, two-segment + `__global__` routing, 409/502 stub routes.
   Unit-tested (`internal/facade`, `internal/supervisor`).
2. **M2 — `omac serve` + sandboxed inner command. ✅ done.** Wraps
   `opencode serve` via `sandbox.ExecWithReady` (§5.3) with the control plane
   running concurrently; single shared sandbox; Option B `--root` policy
   (§5.4); cold start that **activates global skills once under
   `/__global__/`** (§4.5, §5.1); control-plane HTTP server;
   `POST /__omac__/activate` that namespaces + spawns + routes workdir-local
   skills; `--workdir` auto-activation (§5.5); `--no-inner` headless driver.
3. **M3 — Auto-register + pending-credentials + layer split. ✅ done.** Lazy
   auto-register of **workdir-local** skills on activate (global skills are
   not re-registered, §5.2 step 3); 409 stub routes; `reload` promotion (via
   deactivate+reactivate); manifest endpoint emitting the local∪global union
   with `scope` (§6.3); single-dir flat aliases (§5.5).
4. **M4 — OpenCode integration.** OpenCode calls `activate` on project open
   and consumes the manifest; verify against the `echo-rest` skill end-to-end
   under two directories. *(Pending real OpenCode hook — the control-plane
   API + manifest are in place and curl-drivable today.)*
5. **M5 — Idle teardown + polish.** `deactivate` ✅; idle-dir GC, session→dir
   token binding (§8.1.3), per-sidecar OS confinement (§8.2 L3), `doctor`
   support for serve mode — *remaining.*

---

## 10. Open questions for review

1. **nono live remount (§5.4 option C).** Is there *any* way to extend a
   running nono sandbox's filesystem allow-list, or must we restart the
   sandbox to add a project root? This decides whether truly-unknown-up-front
   project roots are achievable or whether Option B's pre-declared roots are
   the ceiling.
2. **Per-dir secret isolation (§8.2, L1) + global carve-out.** Spec keys
   workdir-local secrets by `(workdir, skill)` in v1, while **user-global
   skills keep the unscoped `omac/<skill>` key and one shared sidecar under
   `/__global__/`** (§4.5). Confirm the carve-out and the reserved `__global__`
   namespace are the desired sharing model (vs. e.g. aliasing the global skill
   under every dir token).
3. **Session→dir token binding (§8.1.3).** Ship the `X-Omac-Dir-Token`
   header binding in v1 (defense in depth), or rely on the random path token
   alone? Depends on whether OpenCode can attach a per-session token to its
   outbound skill calls.
4. **Hostile-sidecar containment (§8.2, L3).** Confirm L3 (per-sidecar OS
   sandbox) is explicitly out of scope for v1 and tracked as future work, and
   that the documented residual risk (a sidecar reading other dirs' files) is
   acceptable for the Desktop deployment.
5. **Control-plane trigger.** Does OpenCode have (or can it gain) a hook to
   call `POST /__omac__/activate` on project open? If not, M2/M3 ship with the
   push-fallback (config roots / hint file) and we wire the pull path in M4.
6. **Single facade port vs. nono `--open-port`.** Confirm one TCP port is
   still sufficient with namespacing (it should be — namespacing is purely
   path-based), so the `--open-port` story is unchanged.
7. **`opencode serve` lifecycle.** Does the server expose a clean
   shutdown/health signal omac can observe, so M5 idle-GC and teardown are
   well-defined?
8. **Defaults + serve-mode auto-register (§4.4 × §5.2).** Lazy activation
   auto-registers a dir's skills without a human present. Should that path
   behave like an implicit `--defaults` (silently adopt global defaults, and
   leave any field with no default as `pending-credentials`)? Proposed: yes —
   auto-register = `--defaults` semantics, with missing-no-default values
   surfacing via the 409 `pending-credentials` route rather than prompting.
   Confirm.
9. **Single-dir secret keying for `start` (§5.5).** The §4.3/§8.2-L1 move to
   `(workdir, skill)` secret keys is scoped to `serve`/multi-dir paths.
   Should plain `omac start` *also* migrate to per-workdir secret keys (for
   consistency and to harden the single-dir case), or stay on the legacy
   name-keyed `omac/<skill>` for backward compatibility? If migrating, a
   read-old-write-new fallback is needed so existing keychain entries aren't
   orphaned.
10. **Cross-mode env-var aliasing (§5.5).** Confirm `serve` should emit the
    unprefixed `OMAC_<SKILL>_BASE` alias when exactly one dir is active (so a
    `SKILL.md` is portable between `start` and `serve`), accepting that the
    alias disappears once a 2nd dir activates. Alternative: never alias, and
    require all `serve`-targeted skills to read the manifest (§6.3) — cleaner
    but breaks drop-in reuse of existing single-dir skills.
