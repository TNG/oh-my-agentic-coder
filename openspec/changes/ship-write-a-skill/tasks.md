## 1. Author the omac-write-a-skill bundle (guidance-only)

- [x] 1.1 Create `internal/builtinskills/assets/omac-write-a-skill/SKILL.md`: frontmatter (`name: omac-write-a-skill`, omac-scoped `description`, metadata `author: tngtech` + `omac-builtin: true`) and a concise body pointing at the bundled guide
- [x] 1.2 Add `internal/builtinskills/assets/omac-write-a-skill/references/creating-a-skill.md` as a byte-exact copy of repo-root `CREATING_A_SKILL.md`
- [x] 1.3 Add a test asserting the bundle copy is byte-identical to repo-root `CREATING_A_SKILL.md` (single source of truth)

## 2. Embed + materialize helpers

- [x] 2.1 Create `internal/builtinskills` package: `//go:embed` the assets tree (`embed.FS`); expose the skill name(s) and a sub-FS accessor
- [x] 2.2 Add a materialize helper that writes a bundle tree into a target skills dir (per-file atomic write, preserve modes, create parents)
- [x] 2.3 Add marker helpers: detect the `omac-builtin` marker on an existing on-disk bundle; classify a target as created / updated / unchanged / foreign

## 3. Provisioning: auto-on-launch + explicit `omac setup`

- [x] 3.1 Add `omac setup` to the CLI dispatch (`internal/cli/`) with usage text and an optional `[harness]` positional to narrow (default: all installed)
- [x] 3.2 Add a per-harness native global skills dir resolver in `internal/config` (`~/.config/opencode/skills` honoring `$XDG_CONFIG_HOME`; `~/.claude/skills`)
- [x] 3.3 `omac setup`: detect installed harnesses (inner command on `PATH`); provision to each; if none detected, provision to all known harnesses with a warning
- [x] 3.4 Materialize marker-guarded: refresh omac-owned dirs to the embedded version; skip + warn on a foreign same-named dir (unless `--force`); report created/updated/unchanged/skipped per harness
- [x] 3.5 Auto-provision on launch: `omac start`/`serve` idempotently materialize the bundle into the active harness's dir; silent when current; never block the launch

## 4. Discoverability (doctor report)

- [x] 4.1 Have `omac doctor` report whether the built-in bundle is present/current per installed harness and point at `omac setup`
- [x] 4.2 (Superseded by 3.5: launch auto-provisions rather than only warning.)

## 5. Docs

- [x] 5.1 Update README Quickstart: built-in skills auto-provision on launch (no extra step); document `omac setup` as the optional all-harness refresh
- [x] 5.2 Update README skills sections + `CREATING_A_SKILL.md` to describe the shipped guidance-only `omac-write-a-skill` skill and the retired `opencode-nono/install.sh` path

## 6. Tests & verification

- [x] 6.1 Unit-test the materialize/marker helpers (files written, modes, created/updated/unchanged classification)
- [x] 6.2 Test the marker guard: a foreign same-named directory is not overwritten and triggers a warning
- [x] 6.3 Test `omac setup` idempotency: a second run reports unchanged and makes no edits
- [x] 6.4 Test the per-harness native global skills dir resolver (OpenCode under `.config`, Claude Code under `~/.claude`, `$XDG_CONFIG_HOME` honored)
- [x] 6.5 Test the `CREATING_A_SKILL.md` ↔ bundle sync guard (1.3)
