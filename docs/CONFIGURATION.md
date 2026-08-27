# Configuration

omac uses several configuration files. None are required — compiled-in
defaults work out of the box — but you can override them as needed.

## Launcher config

`oh-my-agentic-coder.yaml` controls sandbox profiles and facade tuning.
omac looks for it in two locations (first found wins):

| Layer | Path |
|---|---|
| Workdir-local | `<workdir>/.opencode/oh-my-agentic-coder.yaml` |
| User-global | `~/.config/omac/config.yaml` (`$XDG_CONFIG_HOME` honored) |

If neither file exists, `DefaultLauncherConfig()` is used (profile
`builtin`, 300 s idle timeout, 10 MB max body).

```yaml
sandbox:
  default_profile: builtin          # or nono, nono-netprofile, no-sandbox-debug
  profiles: { }                     # override or add profiles; defaults are merged
facade:
  idle_timeout_secs: 300
  max_body_bytes: 10485760
  base_env_passthrough: [PATH, HOME, USER, LANG, LC_ALL, LC_CTYPE, TMPDIR]
audit:
  enabled: true                     # security audit trail (default on)
  path: ""                          # "" => persistent central path (see below)
  syslog: false                     # also mirror events to the system log (Unix)
  strict: false                     # fail-closed: abort if the log can't be written
```

## Audit trail

omac records a security audit trail: an append-only, structured
(JSON Lines) log of every security-relevant action it performs — the
sandboxed inner command and each sidecar it spawns (`process.exec` /
`process.exit`), outbound network allow/deny decisions and their source
(`net.decision`), facade requests (`facade.request`), control-plane
mutations (`control.mutation`), secret injection by name
(`secret.inject`), non-ready routes (`route.state`), and session
lifecycle (`session.start` / `session.stop`).

The log lives at a **persistent, central** location that survives
restarts (successive runs append, distinguished by a per-run `run_id`):

| Platform | Default path |
|---|---|
| Linux | `$XDG_STATE_HOME/omac/audit/audit.jsonl` → `~/.local/state/omac/audit/audit.jsonl` |
| macOS | `~/Library/Logs/omac/audit/audit.jsonl` |

The directory is `0700` and the file `0600`, and the default location is
**outside** the sandbox's writable grants, so the confined process cannot
tamper with the host's audit trail. Secret values and per-directory
namespace tokens are **never** written verbatim: secrets are logged by
name only, and namespaces are hashed (`ns_…`).

Flags (precedence: flag > config > default):

| Flag | Effect |
|---|---|
| `--audit-log <path>` | Write the log to `<path>` instead of the default. |
| `--no-audit` | Disable the audit trail. |
| `--audit-strict` | Fail-closed: refuse to start if the log can't be opened, and abort the run if a write fails mid-session. (Cannot be combined with `--no-audit`.) |

By default writing is **fail-open**: a write error degrades to a single
stderr warning and the run continues. Use `--audit-strict` (or
`audit.strict: true`) for a compliance/forensics posture where an
unrecorded run is unacceptable. Enable `audit.syslog` for a tamper-
resistant out-of-band copy via the system log.

The log is a single growing file; use `logrotate` (Linux) or `newsyslog`
(macOS) for rotation — the append-only JSON Lines format is
rotation-safe.

## Skill registry

`sidecar.json` records which skills are registered (name, directory,
bundle hash, declared secrets). It lives in two layers, merged at
startup with workdir winning on collision:

| Layer | Path |
|---|---|
| Workdir-local | `<workdir>/.opencode/sidecar.json` |
| User-global | `~/.config/omac/sidecar.json` |

Written by `omac register` / `omac deregister`; read by `omac start`,
`omac list`, `omac doctor`. **Not mounted into the sandbox.**

## Skill config

`skill-config.yaml` stores non-secret per-skill fields (API base URLs,
region names, feature flags — anything safe to commit). Same two-layer
merge as the registry:

| Layer | Path |
|---|---|
| Workdir-local | `<workdir>/.opencode/skill-config.yaml` |
| User-global | `~/.config/omac/skill-config.yaml` |

Written by `omac register` (prompts for fields) and `omac config`;
read by `omac start` to inject field values into sidecar env vars.
**Not mounted into the sandbox** — resolved values are passed as
environment variables.

## Sandbox profiles

The built-in sandbox reads JSON profiles from
`~/.config/omac/sandbox-profiles/`. On first `omac start` with the
`builtin` profile, omac scaffolds `default.json` from the compiled-in
defaults so you can edit it (only the launch itself writes it — the
inspection commands `omac doctor`, `omac diagnose` and `omac provenance`
read the compiled-in defaults instead of creating files):

```
~/.config/omac/sandbox-profiles/
├── default.json              # filesystem grants, network mode, protected paths
└── default.pages.json        # learned allow/deny decisions (network prompts)
```

Profile fields: `workdir.access` (none/read/write/readwrite),
`filesystem.allow` / `.read` / `.write` (path grants, `~` and `$VAR`
expansion), `filesystem.deny` (mask files inside granted trees — a
bare name like `.env` or `*.key` is denied in every granted directory,
the working directory included), `filesystem.override_deny` (punch
holes in the built-in protected-path list),
`filesystem.registry_config` (see below), `network.mode`
(filtered/blocked/open), `network.network_prompt`,
`network.proxy_injection`, and `environment.allow_vars`. See the
scaffolded `default.json` for the full schema.

### Private package registries (`filesystem.registry_config`)

A scoped package resolves through a mapping in your package manager's
user config — `@acme:registry=https://npm.acme.test` in `~/.npmrc`. That
file is a protected path, because it also commonly holds an auth token.
Masked, it makes npm fall back to the public registry, where the package
does not exist: the install fails with a **404 that reads like "no such
package"** rather than "your registry configuration is invisible".
Allowlisting the registry host does not help — the tool never asks it.

Opting in projects a credential-free copy instead:

```json
{ "filesystem": { "registry_config": ["npm"] } }
```

At launch omac derives a copy of `~/.npmrc` holding **only** registry
mappings, grants read access to that copy alone, and points npm at it via
`NPM_CONFIG_USERCONFIG`. The host file stays masked. No secret can reach
the sandbox: every non-mapping entry is dropped, inline URL credentials
(`https://user:pass@host`) are stripped, and a mapping whose URL carries a
query string or fragment is **refused** rather than copied — that is where
an API key usually hides (`?apiKey=…`), and omac cannot tell a secret
parameter from a load-bearing one.

npm's own value syntax is honored first, so a mapping npm would act on is
not lost: surrounding quotes are removed, and `${VAR}` is expanded — but
**only inside the URL's authority**, so the corporate
`https://${ART_HOST}/api/npm/` shape works while
`https://host/api/${SECRET}/npm/` is refused. A path segment is not
strippable the way userinfo is, and omac cannot tell an interpolated secret
from an interpolated path.

Two cases are deliberately **not** projected, and both are reported at
launch and by `omac doctor` rather than skipped quietly:

- A mapping omac cannot turn into a credential-free `http(s)` URL.
- The **global** `registry` key when the file also holds a credential for
  that host. omac cannot supply the token, and redirecting all resolution
  to a registry it cannot authenticate to would break even the public
  installs that work today. A *scoped* mapping to such a host is still
  projected — that scope was already failing — with a warning that installs
  may return 401/403.

Private registries usually also need their host in
`network.allow_domain` (or an allow at the network prompt).

> **Sharing a profile across machines.** Profiles are parsed strictly —
> an unknown field is an error, so a typo cannot silently weaken the
> sandbox. The trade-off is that a profile using `registry_config` is
> rejected by an omac older than the release that added it. If you share or
> check in a profile, upgrade omac everywhere before adding the field.

The blunt alternative — `override_deny: ["~/.npmrc"]` — also works, but
grants the whole file including any token. `omac doctor` flags a private
mapping the sandbox cannot see, and warns when `override_deny` is doing
a job the projection would do without the exposure.

When a host is neither allowed nor denied by the profile, the network
prompt dialog offers eleven choices (`Deny once` is preselected):

| Choice | Applies to | Lifetime |
|---|---|---|
| `Allow once` / `Deny once` | this request | the request |
| `Allow for this session (this host)` / `Deny …` | that exact host | this `omac start` run |
| `Allow for this session (*.example.com)` / `Deny …` | the suffix and its subdomains | this `omac start` run |
| `Allow permanently (this host)` / `Deny …` | that exact host | until you edit the file |
| `Allow permanently (*.example.com)` / `Deny …` | the suffix and its subdomains | until you edit the file |
| `Explain more` | this request | never stored |

Only the permanent choices are written to disk, in `<profile>.pages.json`
next to the sandbox profile. Session choices are held in memory by the
sandbox supervisor and vanish when the run exits — nothing on disk holds
them, so they cannot be edited or undone from a file; restart the sandbox
to clear one.

Session choices sit between the profile lists in precedence: a session
**deny** outranks `allow_domain`, while a session **allow** never
overrides `deny_domain` or a permanent deny. `omac diagnose` names a
session decision as the cause when one shadows an `allow_domain` entry.

The dialog is sized to fit all eleven rows without scrolling. On unusual
displays (720p laptops, tiling window managers, HiDPI) set
`OMAC_PROMPT_WIDTH` / `OMAC_PROMPT_HEIGHT` in pixels to override the
defaults; a missing or non-numeric value falls back to them.

**`network.proxy_injection`** routes *proxy-unaware* toolchains through
the omac filtering proxy under `network.mode: filtered`. Most tools
(`curl`, `git`, `pip`, `npm`/`yarn`/`pnpm`, `go`) already honor
`HTTP(S)_PROXY` and need nothing here; this option is for families that
ignore those vars:

```jsonc
// ~/.config/omac/sandbox-profiles/default.json
"network": { "mode": "filtered", "proxy_injection": ["jvm", "node"] }
```

- **`jvm`** — injects a supervisor-controlled `JAVA_TOOL_OPTIONS` so
  Gradle/Maven/sbt/Kotlin/`java` route through the proxy. Host/port
  routing applies to every JVM; only Gradle and Maven authenticate the
  proxy `CONNECT` tunnel (they parse the `proxyUser`/`proxyPassword`
  sysprops). Tools on the core JDK HTTP client — including Gradle's
  **Java-toolchain auto-download** (Foojay) — route but do **not**
  authenticate and get a `407`; provisioning a toolchain over an
  authenticated proxy is out of scope.
- **`node`** — injects `NODE_USE_ENV_PROXY=1` for Node's built-in
  `fetch`/`http`. Requires **Node ≥ 22.21.0 on the 22.x line, or ≥ 24.5.0 on current and later lines**; on older runtimes omac emits a
  warning and does not claim routing (the npm/yarn/pnpm CLIs work
  without this family).

**The Gradle daemon needs one extra grant.** The daemon talks to its
client over a *random loopback port*, which the default
`network.enforcement: kernel` does not permit. Prefer
`./gradlew --no-daemon` (or `org.gradle.daemon=false`) — nothing else is
needed. If you must keep the daemon, the fix is platform-specific:

| Platform | Setting | Egress still kernel-enforced? |
|---|---|---|
| macOS | `"network": { "open_port": [0] }` | **yes** — `0` is the "any loopback port" sentinel, which Seatbelt can express as `localhost:*` while still denying external egress |
| Linux | `"network": { "enforcement": "env-only" }` | no — Landlock rules are address-blind, so "any loopback port" would have to mean any port; the proxy filter and prompt still apply, but the kernel bypass-guarantee is gone |

On macOS prefer `open_port: [0]` over `enforcement: env-only` — it solves
the same daemon problem without opening general network access. The `0`
sentinel is **macOS-only**: on Linux it is a silent no-op (there is no
Landlock rule that can express it), so Linux needs `env-only`.

**Numeric `open_port` on Linux is also address-blind.** Landlock's
net-port rules have no host dimension: granting `"open_port": [3000]`
(or `omac start --open-port 3000`) allows bind **and** connect on port
3000 to *any* host, not only loopback. Prefer pinning a single known
dev-server port and keep the list short. `omac doctor` / `omac provenance
--check` flag every numeric `open_port` as a low-severity finding for
this reason.

## Browser tests (Playwright / Puppeteer)

Headless Playwright or Puppeteer suites can run **inside** the omac
sandbox when the sandbox profile grants the browser binary paths and the
local webServer port is opened.

**Profile grants** (add to `~/.config/omac/sandbox-profiles/<name>.json`):

```json
"filesystem": {
  "read": [
    "$PLAYWRIGHT_BROWSERS_PATH",
    "~/.cache/ms-playwright",
    "~/.cache/puppeteer",
    "~/Library/Caches/ms-playwright"
  ]
},
"environment": {
  "allow_vars": ["PLAYWRIGHT_*", "PUPPETEER_*"]
}
```

A broader `~/.cache` / `~/Library/Caches` allow (as in some org profiles)
already covers the default Playwright/Puppeteer install locations; the
entries above are the minimal set. Org installers that ship a custom
profile should put these grants there — there is no separate CLI overlay.

A Linux integration test exercises the full runner path:

`go test ./internal/sandboxrun/ -run TestIntegrationPlaywrightSmoke -v`

(fixture under `internal/sandboxrun/testdata/playwright-smoke/`; skips when
node/Chromium are missing).

**Port.** Pin the webServer port and grant it at launch or in the profile:

```bash
omac start --open-port 3000
```

```ts
// playwright.config.ts
export default defineConfig({
  webServer: {
    command: 'npm run start',
    port: 3000,          // must match --open-port / profile open_port
    reuseExistingServer: !process.env.CI,
  },
});
```

Or permanently in the profile: `"network": { "open_port": [3000] }`
(see the Landlock address-blind caveat above).

**Chromium launch flags.** Inside omac you do **not** need
`--no-sandbox` or `--disable-dev-shm-usage`. Modern Chromium uses its
user-namespace sandbox inside bwrap, and bwrap's `--dev /dev` already
provides a writable `/dev/shm`. (Playwright already defaults
`chromiumSandbox: false` for its own reasons — that is independent of
omac.)

**Supported target: local loopback only.** Chromium ignores credentials
embedded in `HTTP_PROXY`/`HTTPS_PROXY` URLs, so it cannot authenticate to
omac's filtering proxy. Requests to external hosts from the browser are
denied (`net DENY … missing/invalid proxy token`). Tests that only hit
`127.0.0.1` / `localhost` are unaffected (`NO_PROXY` covers loopback).
If a suite must reach the public internet from the browser, configure
Playwright's own proxy auth from the injected `HTTP_PROXY` URL:

```ts
// optional escape hatch — not required for local webServer tests
const u = new URL(process.env.HTTP_PROXY!);
use: {
  proxy: {
    server: `http://${u.hostname}:${u.port}`,
    username: u.username,
    password: u.password,
  },
}
```

`--open-port` also works on `omac continue`, `omac resume`, and
`omac serve`. It applies only to the native (`builtin`) sandbox backend;
nono profiles ignore it with a warning.

## Corporate proxy

In corporate environments where outbound traffic must go through a proxy,
omac auto-detects the standard proxy environment variables
(`HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` — case-insensitive) from the
host environment. Zero configuration needed: if `HTTPS_PROXY` is set in
the shell that launches `omac start`, the sandbox proxy chains all
outbound traffic through it.

**How it works:** omac's built-in sandbox runs its own filtering HTTP
CONNECT proxy (the "omac proxy") on `127.0.0.1`. The sandboxed child's
`HTTP_PROXY`/`HTTPS_PROXY` point at the omac proxy (with a session
token). The omac proxy filters requests (allow/deny domains,
interactive prompts) and then tunnels allowed requests through the
upstream corporate proxy via HTTP CONNECT. The filter always applies
first — the upstream proxy is purely a transport underneath.

**Override via sandbox profile:** If you need a different upstream
proxy than the host environment provides, set it in the sandbox profile:

```json
// ~/.config/omac/sandbox-profiles/default.json
{
  "network": {
    "upstream_proxy": "http://proxy.corp.example.com:8080",
    "no_proxy": ["internal.corp", "registry.internal"]
  }
}
```

Profile fields take precedence over environment variables.

**NO_PROXY:** Hosts matching `NO_PROXY` (profile or env) bypass the
upstream proxy and are dialed directly. An entry matches that exact
host or any subdomain of it (`internal.corp` also matches
`api.internal.corp`); matching is case-insensitive. CIDR ranges and
leading-dot forms (`.internal.corp`) are not supported. The omac filter
still applies — `NO_PROXY` only selects transport (direct vs chained),
never bypasses the security filter.

**Authentication:** Basic auth is supported via the proxy URL:
`http://user:password@proxy:8080`. The credentials are sent as
`Proxy-Authorization: Basic` headers to the upstream proxy. They are
never logged or included in error responses.

**NTLM / Kerberos:** Not supported directly. Use a local authentication
bridge like [`cntlm`](http://cntlm.sourceforge.net/) or
[`px`](https://github.com/genotrance/px) that handles NTLM/Kerberos
and exposes a plain HTTP proxy with Basic auth. Point
`network.upstream_proxy` (or `HTTPS_PROXY`) at the local bridge.

## Secrets

Secrets (API keys, tokens) are stored in the **OS keychain**
(Keychain on macOS, Secret Service / D-Bus on Linux, Credential
 Manager on Windows) — never on disk. Managed via `omac secrets`.
**Never reachable inside the sandbox.** See
[`SECURITY_MODEL.md`](./SECURITY_MODEL.md) for the full boundary.

