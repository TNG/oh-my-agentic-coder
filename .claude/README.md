# omac Claude Code bridge

`hooks/omac-bridge.sh` is the Claude Code-side counterpart to the OpenCode
plugin (`.opencode/plugins/omac-multidir.ts`). It wires Claude Code, running
inside `omac start claude` / `omac serve claude`, to the omac control plane so
each directory's skills come online and are surfaced to the agent.

It lives in `.claude/`, the project-level Claude Code config directory, and is
registered via `.claude/settings.json` as `SessionStart` and `SessionEnd`
hooks.

## What it does

When Claude Code runs under omac, omac injects `OMAC_CONTROL_BASE` (the
control-plane URL) into the environment. The hook uses it to:

1. **Activate on session start** — on `SessionStart` it `POST`s
   `/__omac__/activate {dir}` (using the session's `cwd`) so that directory's
   skills come online lazily.
2. **Surface skills to the agent** — it renders the skills manifest from the
   activate response and returns it as the `SessionStart` hook's
   `additionalContext`, so the model sees the available skill `base` URLs and
   any `pending-credentials` / `broken` status.
3. **Deactivate on session end** — on `SessionEnd` it `POST`s
   `/__omac__/deactivate {dir}`.

This is the same bridge interface the OpenCode plugin implements (see
`docs/contributing/serve-spec.md` and the `agent-bridge` spec). The control plane,
token minting, route namespacing, sidecar spawning/health-checks, and secret
resolution are all owned by omac, identically for every harness.

## Per-session skill env

Claude Code has no per-shell environment hook equivalent to OpenCode's
`shell.env`, so the bridge does **not** inject distinct `OMAC_D_*` per session.
Instead it relies on the flat single-directory aliases that omac already
exports into the process environment at launch — `OMAC_<MOUNT>_BASE` for
workdir skills and `OMAC_G_<MOUNT>_BASE` (plus the flat alias) for global
skills. Claude Code inherits these, so skills that read those env vars resolve
for the active directory. This is the documented fallback for the common
one-directory-per-process case.

## Degradation

If `OMAC_CONTROL_BASE` is not set (i.e. Claude Code is not running under omac),
every branch of the hook is a no-op — it is inert and safe to keep in any
Claude Code project.

## Requirements

`bash`, `curl`, and ideally `jq`. Without `jq` the hook still activates the
directory; it just skips rendering the manifest into context.
