# omac Pi bridge

`index.ts` is the Pi-side counterpart to the OpenCode plugin
(`.opencode/plugins/omac-multidir.ts`). It bridges Pi, running inside
`omac start pi`, to the omac control plane so each directory's skills come
online and are surfaced to the agent.

It lives in `.pi/extensions/`, the project-level extension directory Pi
auto-loads at startup (per https://pi.dev/docs/latest/extensions).

## What it does

When Pi runs under omac, omac injects `OMAC_CONTROL_BASE` (the control-plane
URL) into the environment. The extension uses it to:

1. **Activate on session start** — on `session_start` it `POST`s
   `/__omac__/activate {dir}` so that directory's skills come online lazily.
2. **Surface skills to the agent** — on `before_agent_start` it renders the
   skills manifest from the activate response and injects it into the
   system prompt, alongside the `OMAC_SANDBOX_BRIEFING` briefing text.
3. **Expose per-skill base URLs** — `OMAC_<MOUNT>_BASE` /
   `OMAC_G_<MOUNT>_BASE` are already in the process environment from omac
   launch, so the extension just needs to not interfere.

This is the same bridge interface the OpenCode plugin implements (see
`docs/MULTI_DIR_DESKTOP.md` and the `agent-bridge` spec). The control plane,
token minting, route namespacing, sidecar spawning/health-checks, and secret
resolution are all owned by omac, identically for every harness.

## Degradation

If `OMAC_CONTROL_BASE` is not set (i.e. Pi is not running under omac),
every branch of the extension is a no-op — it is inert and safe to keep in
any Pi project.

## Requirements

Pi's extension system auto-discovers `.pi/extensions/*.ts` (project-local)
and `~/.pi/agent/extensions/*.ts` (global). The extension uses only bundled
modules (no `package.json` or `npm install` needed).
