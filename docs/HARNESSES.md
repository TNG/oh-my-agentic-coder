# Harnesses

omac is harness-agnostic: it launches an inner agentic coder inside the
sandbox and exposes skills to it through a stable `OMAC_*` / REST contract.
This document covers harness selection, session resume, the per-harness
bridges, and how skill discovery is scoped per harness.

For the weekly upstream CLI-compatibility monitoring, see
[`HARNESS_COMPAT.md`](./HARNESS_COMPAT.md).

## Choosing an inner harness

omac is harness-agnostic: it launches an inner agentic coder inside the
sandbox and exposes skills to it through a stable `OMAC_*` / REST contract. The
harness is selected by an optional **positional token** after `start` / `serve`:

```bash
omac start            # default harness (opencode) — unchanged behavior
omac start opencode   # OpenCode
omac start claude     # Claude Code
omac start codex      # OpenAI Codex CLI
omac start copilot    # GitHub Copilot CLI
omac start pi         # Pi (pi.dev)
omac start codewhale  # CodeWhale (bring-your-own-model)
omac serve claude     # multi-directory server, Claude Code harness
```

Supported harnesses (and aliases): `opencode` (`oc`), `claude-code`
(`claude`, `cc`), `codex` (`cx`), `copilot` (`co`), `pi`, `codewhale`
(`cw`). Omitting the token defaults to `opencode`. An unknown token is rejected with the list
of supported names. Inner arguments that happen to be barewords go after
`--` (e.g. `omac start claude -- --model sonnet`).

### Platform support: codex on macOS

`codex` is **not supported under the omac sandbox on macOS**. Its Rust HTTP
client is incompatible with the macOS Seatbelt sandbox (`sandbox-exec`):
the stream disconnects mid-completion even with `network=open`, so every
model call hangs. `omac start codex` (and `continue` / `resume` / `serve`)
on macOS refuses to start rather than hanging silently. `--no-sandbox` is
**not** a safe workaround — it disables the entire omac sandbox
(filesystem isolation, network egress filtering, secret isolation, env
filtering). Use a different harness (`opencode`, `claude-code`, `copilot`)
or run codex on Linux (bwrap works). See
[issue #48](https://github.com/TNG/oh-my-agentic-coder/issues/48)
for the root-cause analysis.

## Resuming prior work

Two convenience subcommands re-enter earlier sessions through the same
sandboxed launch pipeline as `start`:

```bash
omac continue          # reopen the last session for this folder (opencode)
omac continue claude   # ...with Claude Code
omac continue codex    # ...with OpenAI Codex
omac continue copilot  # ...with GitHub Copilot
omac continue pi       # ...with Pi
omac continue -s <id>  # reopen a specific session by id (shorthand for --session)
omac resume            # pick from this folder's recent sessions, then launch
omac resume claude     # ...with Claude Code
```

`omac continue` re-enters the most recent session for this folder. Pass
`-s`/`--session <id>` to target a specific session non-interactively
(opencode `--session <id>`, claude `--resume <id>`, codex `resume <id>`,
copilot `--session-id <id>`, pi `--session <id>`, codewhale `resume <id>`).
After the inner command exits, omac prints a one-line hint with the most
recent session id:

```
To resume this session: omac continue -s ses_abc123
```

`omac resume` lists sessions newest first and launches the one you pick
inside omac. It reads each harness's own session store — opencode via
`opencode session list`, Claude Code by reading
`~/.claude/projects/<encoded-cwd>/<id>.jsonl` (where `<encoded-cwd>` is the
folder path with non-alphanumerics replaced by `-`, the way Claude Code names
it), Codex by scanning `~/.codex/sessions/`, Copilot by querying
`~/.copilot/session-store.db`. Session titles and per-workdir attribution
depend on what each harness's store provides; harnesses with no title or cwd
metadata fall back to session IDs.
Both subcommands take the same flags and optional `[harness]` token as `start`.

## Harness bridges

Each harness ships a small client-side **bridge** that wires the agent to
omac's control plane (skill activation, the skills manifest, skill base URLs):

| Harness     | Bridge location              | Mechanism                         |
| ----------- | ---------------------------- | --------------------------------- |
| OpenCode    | `.opencode/plugins/`         | OpenCode plugin (`omac-multidir.ts`) |
| Claude Code | `.claude/` (settings + hook) | `SessionStart`/`SessionEnd` hooks |
| Codex       | `.codex/`                    | SessionStart hook                  |
| Copilot     | `.copilot/`                  | SessionStart + SessionEnd hooks    |
| Pi          | `.pi/extensions/`            | TypeScript extension (`omac-bridge`) |
| CodeWhale   | *(none)*                     | Briefing delivered as a rules file |

Skills themselves are **harness-agnostic** — the same skill works unchanged
under any harness. Adding a new agentic harness means registering one
descriptor in `internal/config/harness.go` plus shipping its bridge; no
command-dispatch code changes. The six supported harnesses — OpenCode,
Claude Code, Codex, Copilot, Pi, CodeWhale — are worked examples. See
[`CREATING_A_SKILL.md`](../CREATING_A_SKILL.md) and
[`MULTI_DIR_DESKTOP.md`](./MULTI_DIR_DESKTOP.md).

## Built-in skills

omac ships a small set of **built-in skills** embedded in the binary and
**auto-provisions them on `omac start` / `omac serve`** — no separate step. On
launch, omac idempotently writes them into the active harness's skills directory
(`~/.config/opencode/skills`, `~/.claude/skills`, `~/.codex/skills`,
`~/.copilot/skills`, `~/.pi/agent/skills`, `~/.codewhale/skills`); it stays silent when they're already current and never
overwrites a same-named directory it doesn't own.

Today the only built-in is **`omac-write-a-skill`** — a guidance-only skill
(just a `SKILL.md`, no sidecar) carrying the [`CREATING_A_SKILL.md`](../CREATING_A_SKILL.md) authoring
guide, so the agent can author new omac skills in any project.

Since provisioning happens on launch, `omac doctor` run *before* the first
`omac start`/`omac serve` will report the built-in as `missing` — that's
expected, not a sign anything is wrong, and resolves itself on first launch.

`omac setup` is available to (re)provision **all** installed harnesses at once
or to refresh after upgrading omac (`omac setup [harness] [--force]`), but you
don't need to run it for the everyday flow. (This replaces the old external
`opencode-nono/install.sh` skill-copy step.)

## Harness-scoped skill discovery

Each harness reads `SKILL.md` from its **own** skills directory, and omac
matches that: discovery is scoped to the active harness.

| Harness     | Own skills dir (workdir / global)              |
| ----------- | ---------------------------------------------- |
| OpenCode    | `.opencode/skills` / `~/.config/opencode/skills` |
| Claude Code | `.claude/skills` / `~/.claude/skills`            |
| Codex       | `.codex/skills` / `~/.codex/skills`               |
| Copilot     | `.copilot/skills` / `~/.copilot/skills`           |
| Pi          | `.pi/skills` / `~/.pi/agent/skills`               |
| CodeWhale   | `.agents/skills` / `~/.codewhale/skills`         |
| *(shared)*  | `.agents/skills` / `~/.config/agents/skills`     |

- The active harness scans **its own dir + the shared `.agents/skills`**, and
  **never** the other harness's dir. So `omac start claude` ignores skills that
  live only under `.opencode/skills`, and vice versa. Put a skill in
  `.agents/skills` to share it across all harnesses.
- A skill name can be **registered once per harness** (each pointing at that
  harness's dir); registering for one harness does not disturb the other.
- The marketplace `/install` defaults to the **active harness's** dir (so
  installed skills land where that harness loads them); pass `target_path` to
  override (e.g. `.agents/skills` for a shared skill).

When a skill name is ambiguous at register time, omac stops and asks you to
pick:

```bash
omac register slack                      # if ambiguous, prints the candidates
omac register slack --harness claude     # pick the harness
omac register slack --global             # pick the user-global one over workdir
```

