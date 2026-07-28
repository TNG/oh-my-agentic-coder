# Egress host maps

`<harness>-egress.json` maps an egress **hostname** to the harness subsystem
that dials it and a human-readable *cause*, powering the network prompt's
"Likely cause" line. Because the sandbox proxy never terminates TLS, only
`host`, `port`, and the source process are observable — this map recovers the
*why* from the hostname alone.

Maps ship per harness: `opencode-egress.json`, `claude-code-egress.json`.

## Multi-harness contract

- **One file per harness**, named `<canonical>-egress.json` where `<canonical>`
  is the harness's canonical name in the `config` registry (`opencode`,
  `claude-code`, …) — *not* a binary basename or alias.
- **`For(harness)` keys on the canonical name.** The caller resolves identity
  once through `config.LookupHarness` (in `sandboxrun.harnessName`) so binary
  basenames and aliases (`claude`, `cc`) collapse to `claude-code` before they
  reach the map. This keeps harness identity single-sourced in `config`.
- **The same host can mean different things per harness** — e.g.
  `raw.githubusercontent.com` is a tree-sitter grammar in opencode but the
  `/release-notes` changelog in claude-code. Keying by harness is what keeps the
  cause honest; an unknown harness yields a nil map (no cause line), never a
  wrong one.
- **Adding a harness is a data task:** audit that harness's egress, drop in
  `<canonical>-egress.json`, add one `case` + `//go:embed` in `hostmap.go`.

## Why this exists

In `filtered` network mode, any host not on the allowlist and not yet learned
triggers an interactive prompt (`internal/netproxy/filter.go`, step 5). Many of
opencode's dials are **background, non-user-initiated** dependency fetches
(syntax-highlighting grammars, LSP servers, model catalog, themes). Users get
prompted for a host they never named, with an empty "Agent intent" line — which
reads as suspicious. This map lets the prompt answer "where is this coming
from" without weakening the guardrail by silently allowlisting the hosts.

## What the map can and cannot resolve

- **Cause from hostname: yes (coarse).** opencode partitions egress by host, so
  the hostname is a reliable signal (`opencode.ai` → config/assets, `models.dev`
  → catalog, `raw.githubusercontent.com` → tree-sitter grammar, etc.).
- **Exact URL: no.** TLS is a raw CONNECT tunnel (by design, nono parity); the
  request path is never visible. The cause is inferred from host + source
  process, not read off the wire.
- **opencode-core vs agent-within: not by process.** opencode runs the agent
  loop, its LLM calls, and background housekeeping in the *same* runtime, so PID
  attribution cannot split them (confirmed: one PID dials `api.anthropic.com`,
  `github.com`, and telemetry). PID *can* distinguish opencode-core from a tool
  subprocess it spawns (an LSP server, ripgrep) — hence the `origin` field.

## Field reference

| field | meaning |
|-------|---------|
| `host` / `match` | hostname and match mode (`exact` \| `suffix`) |
| `origin` | harness-specific — see each file's `meta.origin_values` (opencode: `opencode-core`/`opencode-tool`/`provider`/`external`; claude-code: `harness-core`/`provider`/`browser`) |
| `category` | `render`, `language-tooling`, `catalog`, `config-assets`, `auth`, `integration`, `llm-inference`, `telemetry` |
| `cause` | the string a prompt would show as "likely cause" |
| `user_initiated` | `false` = background infra; `true` = agent/user-driven |
| `source` | opencode source call-site(s) backing the attribution |
| `notes` | ambiguity, overrides, redirect targets |

`opt_in` lists hosts dialed only under explicit env config (telemetry).
`not_egress` records hosts that look like opencode traffic but are not — e.g.
`1.1.1.1` appears only in a webfetch test fixture and a code comment; it is
never dialed at runtime. Documented so it is not re-investigated.

## Provenance & regeneration

Provenance differs per harness — each file's `meta` block records its own
sources. Both are **manually authored, not generated.**

### opencode (`opencode-egress.json`)

- Source: `anomalyco/opencode` @ `355a0bcf5` (`v1.17.8`), audited against the
  installed `v1.17.12` — call-site attribution from the real source tree.
- Rebuild by re-auditing call-sites:

```sh
grep -rhoiE "https://[a-z0-9._-]+" packages --include=*.ts --include=*.go \
  | sed -E 's#https://##' | sort | uniq -c | sort -rn
grep -rniE "<host>" packages --include=*.ts --include=*.go | grep -viE "test|dist"
```

Key call-sites: `parsers-config.ts` (grammars/queries), `lsp/server.ts` (LSP
downloads), `models-dev.ts` (catalog), `npm-config.ts` (registry),
`installation/index.ts` (self-update), `observability/otlp.ts` (opt-in
telemetry). Re-verify on opencode bumps: new languages/LSPs add hosts.

### claude-code (`claude-code-egress.json`)

- **Authority:** Anthropic's documented allowlist —
  <https://code.claude.com/docs/en/network-config> ("Network access
  requirements") — corroborated against the Claude Code **v2.1.215** binary and
  a sandbox-audit run.
- **Coarser than opencode's on purpose:** the shipped Claude Code is a minified
  bundle with no source repo, so attribution is host-level from the published
  allowlist, not call-site. Rebuild by re-reading that doc on Claude Code
  releases and diffing the binary's embedded hosts:

```sh
strings -n 6 "$(readlink -f "$(command -v claude)")" \
  | grep -oiE "https://[a-z0-9._-]+" | sed -E 's#https://##' | sort | uniq -c | sort -rn
```

Re-verify when Anthropic updates the network-config allowlist (hosts and their
purposes change across versions — e.g. the installer host moved off
`storage.googleapis.com` in 2.1.116).
