# opencode egress host map

`opencode-egress.json` maps an egress **hostname** to the opencode subsystem
that dials it and a human-readable *cause*. It is the data behind a future
network-prompt enhancement: because the sandbox proxy never terminates TLS,
only `host`, `port`, and the source process are observable — this map recovers
the *why* from the hostname alone.

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
| `origin` | `opencode-core` \| `opencode-tool` \| `provider` \| `external` (see `meta.origin_values`) |
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

- Source: `anomalyco/opencode` @ `355a0bcf5` (`v1.17.8`), audited against the
  installed `v1.17.12`.
- **Manually authored, not generated.** Rebuild by re-auditing call-sites:

```sh
# hosts hardcoded in the running packages
grep -rhoiE "https://[a-z0-9._-]+" packages --include=*.ts --include=*.go \
  | sed -E 's#https://##' | sort | uniq -c | sort -rn

# attribute a host to its subsystem
grep -rniE "<host>" packages --include=*.ts --include=*.go | grep -viE "test|dist"
```

Key call-sites: `packages/tui/src/parsers-config.ts` (grammars/queries),
`packages/opencode/src/lsp/server.ts` (LSP downloads),
`packages/core/src/models-dev.ts` (catalog),
`packages/core/src/npm-config.ts` (npm registry),
`packages/opencode/src/installation/index.ts` (self-update),
`packages/core/src/observability/otlp.ts` (opt-in telemetry).

Re-verify on opencode minor/major bumps: new languages add grammar hosts, new
LSPs add download hosts.
