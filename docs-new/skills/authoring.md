---
title: Writing a skill
description: How to build a new omac skill
---

A skill is a directory containing an `omac.yaml` (runtime contract), a `SKILL.md` ([agentskills.io](https://agentskills.io/home) discovery file), and an optional sidecar script or binary under `scripts/`. For a complete working example, see [echo-rest](../contributing/echo-rest.md).

These two files are all omac needs. A skill published through the marketplace also carries a `meta.yaml`, but that file belongs to the marketplace — omac never reads it, and you do not need one to write or run an omac skill.

## omac.yaml

**Top-level fields**

| Field | Required | Description |
|---|---|---|
| `name` | yes | Must match the directory name and `SKILL.md` frontmatter `name`. |
| `type` | no | Should be `skill`. |
| `version` | no | SemVer string (e.g. `0.1.0`). |
| `description` | no | One-line summary shown in `omac list`. |
| `author` | no | Author name or contact. |
| `dependencies` | no | List of other skill names this skill depends on. |

**`sidecar:` block**

| Field | Required | Default | Description |
|---|---|---|---|
| `command` | yes | — | Argv to spawn the sidecar (e.g. `["python3", "scripts/sidecar.py"]`). Path is relative to the skill root. |
| `mount` | no | `name` | URL prefix on the facade (e.g. `my-skill`). Must match `^[a-z0-9][a-z0-9-]*$`. |
| `env_passthrough` | no | `[]` | List of host env var names to forward into the sidecar. May overlap with a `secrets` entry as a keychain-unavailable fallback, but not with `config`. |
| `protocols` | no | `[]` | Informational list of protocol tags the sidecar speaks. |
| `health.path` | no | `/status` | HTTP path polled for 2xx before routes are mounted. |
| `health.initial_delay_ms` | no | `200` | Milliseconds before the first health poll. |
| `health.timeout_ms` | no | `5000` | Milliseconds before a single health request times out. |
| `health.interval_ms` | no | `500` | Milliseconds between health polls. |
| `install_scripts` | no | — | Map of OS key → relative script path (e.g. `{linux: install/install.linux.sh, macos: install/install.macos.sh}`). Run by `omac install`. |
| `limits.max_body_bytes` | no | — | Maximum request/response body the proxy will forward, in bytes. |
| `limits.idle_timeout_secs` | no | — | Seconds of inactivity before the proxy closes a sidecar connection. |
| `secrets` | no | `[]` | List of credential declarations (see below). |
| `config` | no | `[]` | List of non-secret config field declarations (see below). |

**`secrets[]` entry fields**

| Field | Required | Default | Description |
|---|---|---|---|
| `name` | yes | — | Valid env var name (`^[A-Z_][A-Z0-9_]*$`). Injected into the sidecar at start time. |
| `description` | no | — | Shown at the `omac register` prompt. |
| `required` | no | `true` | Must the field be set? |
| `pattern` | no | — | Regex the value must match. |
| `default_from_env` | no | — | Host env var to pre-fill at the prompt. |
| `multiline` | no | `false` | Allow multi-line input. |

**`config[]` entry fields**

| Field | Required | Default | Description |
|---|---|---|---|
| `name` | yes | — | Valid env var name (`^[A-Z_][A-Z0-9_]*$`). Injected into the sidecar at start time. |
| `description` | no | — | Shown at the `omac register` prompt. |
| `type` | no | `string` | Value type: `string`, `bool`, `int`, or `enum`. |
| `required` | no | `true` | Must the field be set? |
| `default` | no | — | Pre-filled value at the prompt; must be valid for `type`. |
| `default_from_env` | no | — | Host env var to pre-fill at the prompt. |
| `pattern` | no | — | Regex the value must match (`string` only). |
| `choices` | no | — | Allowed values list (`enum` only; required when `type: enum`). |

Minimal `omac.yaml`:

```yaml
name: my-skill
type: skill
version: 0.1.0
description: One-line summary for omac list.

sidecar:
  command: ["python3", "scripts/sidecar.py"]
  mount: my-skill
  health:
    path: /status
  secrets:
    - name: MY_API_TOKEN
      description: Token for the upstream API.
      required: true
  config:
    - name: API_BASE_URL
      type: string
      default: "https://api.example.com"
```

## SKILL.md

`SKILL.md` is YAML frontmatter plus a Markdown body. omac does not parse it — the agent uses it for discovery and activation. The frontmatter fields that matter:

| Field | Required | Notes |
|---|---|---|
| `name` | yes | Must match directory name and `omac.yaml` `name`. |
| `description` | yes | 1–1024 chars. Cover both *what* the skill does and *when* the agent should use it — this is the only thing the agent sees at discovery time. Make it keyword-rich. |
| `license` | no | License name or path to a bundled file. |
| `compatibility` | no | Max 500 chars. State omac runtime version, language runtime, and any network requirements. |

The body is read by the agent when it is actively using the skill — write it as agent-facing documentation. Cover:

- **What the skill does**: expand on the `description` with enough detail for the agent to use it correctly.
- **When to use it**: help the agent decide whether this skill fits the task at hand.
- **How to call it**: the endpoints it exposes, what parameters they accept, and what they return. The agent constructs requests from this.
- **Configuration notes**: any injected config fields the agent should be aware of (e.g. fields that affect behavior).

Keep the body under 500 lines. Move detailed reference material into `references/` and link to it. See [echo-rest](../contributing/echo-rest.md) for a complete example. Validate with `skills-ref validate ./.opencode/skills/<name>`.

## Secrets and config

**Secrets** (`sidecar.secrets`) are credentials. `omac register` prompts with masked input, stores in the OS keychain under `omac/<skill>/<NAME>`, and injects into the sidecar process at start time as env vars.

**Config fields** (`sidecar.config`) are non-secret operational values. Prompted with echoing input, stored in `<workdir>/.opencode/skill-config.yaml` (mode 0600), and injected identically to secrets.

To update stored values after registration:

| Command | What it does |
|---|---|
| `omac register --reprompt-fields <skill>` | Re-prompts for all config fields of a skill. |
| `omac secrets set <skill> <NAME>` | Prompts (masked) for a new value for the named secret and stores it in the keychain. `<NAME>` is the env var name declared under `sidecar.secrets` (e.g. `MY_API_TOKEN`). |
| `omac secrets unset <skill> <NAME>` | Removes a secret from the keychain. |
| `omac secrets list <skill>` | Lists all declared secrets and whether each is present in the keychain. |
| `omac secrets import <skill> --from <file>` | Bulk-imports secrets from a `KEY=VALUE` file into the keychain. |

## Sidecar

The sidecar is an HTTP server that runs outside the agent sandbox. When omac starts your skill, it:

1. Picks a free port and injects it — along with credentials, facade URLs, and other context — into the sidecar process via environment variables.
2. Spawns the sidecar process.
3. Polls the health endpoint until it gets a 2xx, then mounts routes through the facade.

The agent inside the sandbox reaches the sidecar through the facade only — it never has direct access to the sidecar or its credentials.

Your sidecar must:

- **Bind on `127.0.0.1:<SIDECAR_PORT>`, never `0.0.0.0`.** omac picks the port and tells you via `$SIDECAR_PORT` — bind there so omac knows where to find you. Binding on all interfaces would expose the sidecar and its injected credentials beyond the local machine.
- **Implement the health endpoint** at `health.path` (default `/status`; configure in `omac.yaml`). Return any 2xx (e.g. `{"ok": true}`). omac will not serve traffic until this succeeds.
- **Never expose secrets** in response bodies or log lines.

## Environment variables injected into the sidecar

omac sets these in the sidecar process before spawning it. Your sidecar code uses them like any other env var — they are how omac tells the sidecar where to bind, what credentials it has, and how to reach the facade. You do not set them yourself.

For example, if your `omac.yaml` declares a secret `MY_API_TOKEN` and a config field `API_BASE_URL`, they arrive in your sidecar as plain env vars:

```python
import os

token   = os.environ["MY_API_TOKEN"]   # from the OS keychain
api_url = os.environ["API_BASE_URL"]   # from skill-config.yaml

# pass directly to whatever HTTP client you use
```

| Variable | How your sidecar uses it |
|---|---|
| `SIDECAR_PORT` | Bind your HTTP server on `127.0.0.1:<SIDECAR_PORT>`. |
| `SIDECAR_SKILL` | Your skill's name — prefix log lines with it for easier debugging. |
| `OMAC_<MOUNT>_BASE` | Base URL for your skill's routes through the facade (TCP). `<MOUNT>` is your skill's URL prefix — the `name` field uppercased with dashes replaced by underscores (e.g. skill `my-skill` → `OMAC_MY_SKILL_BASE`). Use this to construct URLs pointing back to your own routes. |
| `OMAC_<MOUNT>_SOCKET_BASE` | Same, via Unix socket — lower latency, use when available. |
| `OMAC_SOCKET` | Path to the omac facade Unix socket — use directly if you need facade access outside the sandbox. |
| `OMAC_BASE` | Facade root URL in TCP form — fallback when the Unix socket is not available. |
| `OMAC_WORKDIR` | Absolute path of the project directory omac was invoked in — use when your sidecar needs to read or write project files. |
| Each declared secret name | The credential value (from the OS keychain) — pass to your upstream API client, as in the example above. |
| Each declared config field name | The config value — use for non-secret settings like base URLs, regions, or feature flags. |

## Discovery roots

Place the skill directory under one of these roots. Priority rules: workdir-local beats user-global; within each layer, the harness's own base beats the shared `.agents` base.

**OpenCode** — own base is `.opencode/skills`:

1. `<workdir>/.opencode/skills/<name>/`
2. `<workdir>/.agents/skills/<name>/`
3. `~/.config/opencode/skills/<name>/`
4. `~/.config/agents/skills/<name>/`
5. `~/.opencode/skills/<name>/` (legacy flat layout)
6. `~/.agents/skills/<name>/` (legacy flat layout)

**Claude Code** — own base is `.claude/skills`:

1. `<workdir>/.claude/skills/<name>/`
2. `<workdir>/.agents/skills/<name>/`
3. `~/.config/claude/skills/<name>/`
4. `~/.config/agents/skills/<name>/`
5. `~/.claude/skills/<name>/` (legacy flat layout)
6. `~/.agents/skills/<name>/` (legacy flat layout)

`$XDG_CONFIG_HOME` replaces `~/.config` when set. Use `.agents/skills/` if you want the skill available regardless of which harness is active.

## Pre-shipping checklist

- `SKILL.md` frontmatter has `name` and `description`; `skills-ref validate` passes; `name` matches the directory name and `omac.yaml`.
- `SKILL.md` `description` covers both *what* and *when*; body is under 500 lines.
- `omac.yaml` validates: `omac register --no-secrets <name>` succeeds.
- `command:` in `omac.yaml` is a relative path (e.g. `scripts/sidecar.py`). Sidecar binds on `127.0.0.1:$SIDECAR_PORT` only — verify after `omac start` with `ss -tlnp` (Linux) or `netstat -an` (macOS) and check the address is not `0.0.0.0`.
- `GET <health.path>` (e.g. `GET /status`) returns any 2xx.
- All credentials declared under `secrets:`, not `env_passthrough`.
- No secret value appears in any response body or log line.
- Install scripts (`install/install.macos.sh`, `install/install.linux.sh`) are executable (`chmod +x`), idempotent (run twice, same result), and exit non-zero on missing prerequisites. For a platform you cannot run: `ls -la install/` shows whether the scripts are executable (look for `x` in the permissions column) and `bash -n <script>` checks syntax.
- Smoke-tested end-to-end with `omac start` (sandboxed) and the agent reaching the sidecar via `$OMAC_SOCKET`.
