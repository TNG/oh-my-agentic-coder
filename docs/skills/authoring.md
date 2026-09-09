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
| `install_scripts` | no | — | Map of OS key → relative script path (e.g. `{linux: install/install.linux.sh, macos: install/install.macos.sh}`). omac prints these paths at `omac register` time for you to run yourself; it never executes them. |
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

Config values are converted to string before injection: a `bool` arrives as the string `"true"` or `"false"`, an `int` as its plain decimal string, and an `enum` as the exact chosen value.

To update stored values after registration:

| Command | What it does |
|---|---|
| `omac register <skill> --reprompt-fields` | Re-prompts for all config fields of a skill. |
| `omac secrets set <skill> <NAME>` | Prompts (masked) for a new value for the named secret and stores it in the keychain. `<NAME>` is the env var name declared under `sidecar.secrets` (e.g. `MY_API_TOKEN`). |
| `omac secrets unset <skill> <NAME>` | Removes a secret from the keychain. |
| `omac secrets list <skill>` | Lists all declared secrets and whether each is present in the keychain. |
| `omac secrets import <skill> --from <file>` | Bulk-imports secrets from a `KEY=VALUE` file into the keychain. |

To inspect what omac would inject for a skill, run `omac config show <skill>` (add `--json` for machine-readable output). It prints each config value with its source. Secrets are never shown in full — you get a short fingerprint (a truncated hash like `sha256:a1b2c3…`) that confirms a value is set and lets you check two setups share the same one. `omac config get <skill> <field>` prints one resolved config value.

For non-interactive setup (CI), skip the prompts: pass `--no-fields` / `--no-secrets`, and supply values with `--fields-from <file>` / `--secrets-from <file>`, or `OMAC_CONFIG_<NAME>` environment variables.

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

**Defining routes.** Your sidecar is an ordinary HTTP server, so define a handler for each path your skill needs (for example `GET /status`). The agent reaches it through omac at `$OMAC_MY_SKILL_BASE/status`, where `$OMAC_MY_SKILL_BASE` is the base URL the agent uses to reach your skill (see [How the agent reaches your skill](#how-the-agent-reaches-your-skill) below). 
Before forwarding the request, omac strips your skill's mount prefix (`/my-skill`, the `mount` value from your `omac.yaml`), so your sidecar receives the bare path `/status`. Register your handlers for those bare paths: a handler for `/my-skill/status` would never be reached.

**Streaming responses.** If a route streams results instead of returning them all at once (for example Server-Sent Events), set these in your sidecar's handler yourself: `Content-Type: text/event-stream`, no `Content-Length`, and a flush after each event. What omac does automatically is forward a `text/event-stream` response to the agent without buffering, so each event arrives as you send it. See the `/tick` route in [echo-rest](../contributing/echo-rest.md) for a working example.

**Python sidecars.** `http.server`'s `ThreadingHTTPServer` does a reverse-DNS lookup when it binds, which can stall startup by about 35 seconds on macOS. echo-rest's `sidecar.py` shows the one-line `server_bind` override that avoids it.

## Environment variables injected into the sidecar

omac sets the injected variables in the sidecar process before spawning it.

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
| `OMAC_WORKDIR` | Absolute path of the project directory omac was invoked in — use when your sidecar needs to read or write project files. |
| Each declared secret name | The credential value (from the OS keychain) — pass to your upstream API client, as in the example above. |
| Each declared config field name | The config value — use for non-secret settings like base URLs, regions, or feature flags. |

## How the agent reaches your skill

The agent uses the variables it receives to reach your skill through the facade:

| Variable | Value |
|---|---|
| `OMAC_<MOUNT>_BASE` | TCP base URL for your skill, e.g. `http://127.0.0.1:<port>/my-skill`. `<MOUNT>` is your `mount` value uppercased with dashes turned into underscores (`my-skill` → `OMAC_MY_SKILL_BASE`). **Prefer this form.** |
| `OMAC_<MOUNT>_SOCKET_BASE` | The same route over omac's Unix socket. It has lower overhead, but the socket is blocked under the macOS sandbox, so use it only where you know it is reachable, not as the default. |

Write your endpoint examples in `SKILL.md` using `$OMAC_<MOUNT>_BASE` so the agent calls your skill over the preferred transport.

Skills are harness-agnostic.

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

The other harnesses follow the same pattern with their own base directory (plus a user-global equivalent): **Copilot** `.copilot/skills`, **Codex** `.codex/skills`, **Pi** `.pi/skills`, **CodeWhale** `.agents/skills`. The shared `.agents/skills` is in scope for every harness.

`$XDG_CONFIG_HOME` replaces `~/.config` when set. Use `.agents/skills/` if you want the skill available regardless of which harness is active.

## Registering: the skill snapshot

When you run `omac register`, omac copies your skill directory. It always runs the sidecar from that copy, never from your project directory. Two practical rules follow:

- **Install dependencies before you register.** Anything added afterwards (a `pip install`, `npm install`, or vendored library) is not in the copy, so the sidecar won't find it. Run `omac register` again after changing dependencies.
- **Don't write files relative to the current directory.** They land in omac's private copy, which is invisible to the agent and discarded on the next register. To read or write files in the project, use the absolute path in `$OMAC_WORKDIR`.

## What `omac start` checks

Before launching, `omac start` compares the skills on disk against what you registered. It refuses to start, and prints the exact fix, if it finds any of:

- An **unregistered** skill directory. Run `omac register <skill>`.
- A registered skill whose files **changed since you registered it** (bundle drift). Re-register with `omac register <skill> --force`, or pass `omac start --accept-skill-changes` to run the edited files as they are.
- A **required config field** with no value. Run `omac register <skill> --reprompt-fields`.

If a registered skill's directory has been deleted, omac auto-deregisters it and continues.

## Develop and test

To iterate quickly, run everything outside the sandbox with a shell in place of the agent:

```bash
omac register my-skill                 # register first, and again after each change
omac start --no-sandbox --inner bash
# then, in the shell it drops you into:
curl "$OMAC_MY_SKILL_BASE/status"
```

`--no-sandbox` skips the OS sandbox and runs your command directly. `--inner bash` replaces the agent with a shell, so you can call the skill by hand. Your sidecar and the facade still start normally.

If a call returns HTTP `503` with the header `X-Omac-Reason: sidecar-down`, your sidecar failed to start or crashed. Its output (stdout and stderr) is captured in a per-project runtime directory under `$TMPDIR`:

```bash
# omac creates one omac-<hash> directory per project; if only one is running:
cat $TMPDIR/omac-*/logs/<skill>.log
```

Because omac runs the skill from the copy made at register time (see above), re-run `omac register` after editing the skill so your changes take effect.

## Pre-shipping checklist

- `SKILL.md` frontmatter has `name` and `description`; `skills-ref validate` passes; `name` matches the directory name and `omac.yaml`.
- `SKILL.md` `description` covers both *what* and *when*; body is under 500 lines.
- `omac.yaml` validates: `omac register --no-secrets <name>` succeeds.
- `command:` in `omac.yaml` is a relative path (e.g. `scripts/sidecar.py`). Sidecar binds on `127.0.0.1:$SIDECAR_PORT` only — verify after `omac start` with `ss -tlnp` (Linux) or `netstat -an` (macOS) and check the address is not `0.0.0.0`.
- `GET <health.path>` (e.g. `GET /status`) returns any 2xx.
- All credentials declared under `secrets:`, not `env_passthrough`.
- No secret value appears in any response body or log line.
- Install scripts (`install/install.macos.sh`, `install/install.linux.sh`) are executable (`chmod +x`), idempotent (run twice, same result), and exit non-zero on missing prerequisites. For a platform you cannot run: `ls -la install/` shows whether the scripts are executable (look for `x` in the permissions column) and `bash -n <script>` checks syntax.
- Smoke-tested end-to-end with `omac start` (sandboxed) and the agent reaching the sidecar via `$OMAC_<MOUNT>_BASE`.
