# CLI reference

Every subcommand, its flags, the typical end-to-end workflow, and the exit
codes omac uses.

## Typical workflow

```bash
# 1. Register a skill in this workdir. Prompts for every declared secret
#    (masked input, stored in the OS keychain; nothing touches disk under .opencode/).
#    The skill directory must already exist under .opencode/skills/, .agents/skills/,
#    or the user-global skills dir.
omac register slack

# 2. Inspect the install script (omac never runs it for you).
bash .opencode/skills/slack/install/install.macos.sh

# 3. (Optional) status.
omac doctor
omac list
omac secrets list slack

# 4. Launch the full stack: sidecars → facade (Unix socket) → sandbox → agent.
omac start            # default harness (opencode)
# or: omac start claude   # launch Claude Code as the inner harness instead
# or: omac start codex    # launch OpenAI Codex as the inner harness
# or: omac start copilot  # launch GitHub Copilot as the inner harness

# Inside the sandbox the skill reaches its sidecar via the socket:
#   curl --unix-socket "$OMAC_SOCKET" http://x/slack/api/chat.postMessage ...

# 5. Rotate a secret without re-registering.
omac secrets set slack SLACK_BOT_TOKEN
```

## CLI summary

```
omac [--workdir <dir>] <subcommand> [flags] [args]

  register     Locate the skill (workdir-local first, then user-global;
               within each layer, .agents/skills ranks above the legacy
               .opencode/skills — see CREATING_A_SKILL.md §2 for the
               full search order including XDG and legacy fallbacks),
               validate meta, prompt for secrets → keychain, prompt for
               config fields → skill-config.yaml, surface the install
               script path (omac never runs it), add to sidecar.json.
               Flags:
                 --force                 replace existing registry entry
                 --reprompt-secrets      re-prompt even if secrets exist
                 --no-secrets            skip all secret prompts
                 --secrets-from <file>   KEY=VALUE file instead of prompting
                 --reprompt-fields       re-prompt config fields
                 --no-fields             skip all config-field prompts
                 --fields-from <file>    KEY=VALUE file for fields

  deregister   Remove a skill. If it is registered, the registry entry is
               removed (its source files are kept). If it was never
               registered but still exists on disk (so `omac start` keeps
               flagging it), its source directory is deleted instead — after
               a confirmation prompt, or immediately with --yes. Flags:
                 --global                force removal from the user-global
                                         registry (~/.config/omac)
                 --harness <name>        remove only one harness's entry
                 --yes                   delete an unregistered skill's source
                                         directory without prompting
                 --purge-secrets         also delete from keychain
                 --purge-fields          also delete from skill-config.yaml
                 --purge-defaults        also delete remembered global defaults
                 --prune                 remove ALL stale registrations
                                         (workdir + global) whose skill
                                         directory no longer exists

  list         Show registered skills with mount, secret count, binary status.
               Registrations whose skill directory no longer exists are
               hidden from the live list and reported separately as "stale"
               with the exact `omac deregister` command to remove them; pass
               --all to include stale rows in the table.

  secrets <sub> <skill> [name]
    list, set, unset, import --from <file>

  config <sub> <skill> [args]
    show <skill> [--json]   resolved config + secret fingerprints
    get  <skill> <field>    one resolved value, suitable for $(...)

  provenance   Show effective allow/deny entries across network, filesystem,
               environment, and skills, plus the default persistent
               tool-cache scope for this workdir (scope, mode, path, and
               the eight-variable cache environment map). Flags:
                 --profile <ref>     sandbox profile name/path/builtin
                 --json             emit JSON instead of tables
                 --check            emit findings for risky profile grants
                                    instead of the view

  start        Spawn sidecars → bind socket → exec sandbox runtime. Refuses
               to start if any skill is unregistered in any of the search
               roots (workdir-local .agents/skills + .opencode/skills,
               plus the user-global layers), or if a registered skill's
               bundle changed since register, or if a required config
               field is unresolvable. `--auto-register-skills` is an
               opt-in for this start-family launch: it silently registers
               only workdir-local skills whose required config and secrets
               resolve without prompting. Other unregistered skills still
               refuse launch and print their registration command.
               Auto-deregisters
               (silently) skills whose directory has vanished; secrets +
               config persist for safety. Flags:
                 --sandbox <profile>     pick a sandbox profile
                 --inner <cmd>           override inner_cmd
                 --no-sandbox            debug: run inner cmd directly
                                         (disables the entire omac sandbox;
                                         no cache scope is prepared)
                 --ephemeral-cache       use a per-launch cache dir instead
                                         of the persistent workdir cache
                                         scope; removed on exit
                 --keep-running          don't stop sidecars on exit
                 --accept-skill-changes  tolerate bundle_hash drift
                 --auto-register-skills  silently register eligible
                                         workdir-local skills only
                 --skip-secret-pattern   don't enforce a secret's pattern
                                         on an env_passthrough value
                 --verbose               lifecycle logging (prints cache
                                         mode/path, sandbox TMPDIR,
                                         control-plane URL, sandbox argv)

   continue     Like `start`, but continue the most recent session for this
                workdir (appends the harness's continue flag: opencode/claude
                `--continue`, codex `resume --last`, copilot `--continue`, pi `-c`).
                Pass `-s`/`--session <id>` to target a specific session
                (opencode `--session <id>`, claude `--resume <id>`, codex
                `resume <id>`, copilot `--session-id <id>`, pi `--session <id>`).
                Accepts the same flags as `start` and an optional [harness]
                token. After exit, prints an `omac continue -s <id>` hint
                when a resumable session exists for this workdir.

  resume       List recent sessions for this workdir, show an interactive
               numbered picker (title + relative time), and launch the
               selected one inside omac (opencode `--session <id>`, claude
               `--resume <id>`, codex `resume <id>`, copilot
               `--session-id <id>`, pi `--session <id>`). Sessions come from
               the harness's own store (opencode `session list`; Claude Code's
               ~/.claude/projects files; codex's ~/.codex/sessions/ rollout
               files; copilot's ~/.copilot/session-store.db; pi's
               ~/.pi/agent/sessions/ JSONL files). Non-interactive stdin
               prints the list and exits.
               Accepts the same flags as `start` and an optional [harness].

  doctor       Sanity checks: config, registry, binaries, secrets, sandbox.
               Also warns (advisory, never mutates the profile) about broad
               cache-root / tool-home grants and about host cargo config /
               credentials files an isolated CARGO_HOME won't pick up.

  cache        Manage the persistent tool cache under ~/.cache/omac.
                 clear           remove the active cache scope (per cache.scope)
                 clear --all     remove every inactive cache scope
                                 (destructive; active scopes are skipped)

  version
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success |
| `1` | generic failure |
| `2` | misuse / invalid arguments |
| `3` | configuration or metadata invalid |
| `4` | prerequisite missing (skill not installed) |
| `5` | I/O error |
| `6` | sidecar failed health check |
| `7` | sandbox exited abnormally |
| `8` | keychain access failed |
| `9` | required secret refused by user |
| `10` | downloaded artifact failed its checksum verification |

