# omac multi-directory plugin

`omac-multidir.ts` is the OpenCode-side counterpart to `omac serve`
(see `docs/MULTI_DIR_DESKTOP.md`). It lives in `.opencode/plugins/`, the
project-level plugin directory OpenCode auto-loads at startup (per
https://opencode.ai/docs/plugins — note the directory is `plugins/`, plural;
the `opencode.json` `"plugin"` array is for **npm packages only**, not local
file paths).

## What it does

When OpenCode runs as `opencode serve` inside `omac serve`, omac injects
`OMAC_CONTROL_BASE` (the control-plane URL) into the environment. This plugin
uses it to:

1. **Activate on directory open** — on `session.created` / `session.updated`
   it `POST`s `/__omac__/activate {dir}` so that directory's skills come
   online lazily (spec §5.2 pull trigger).
2. **Surface skills to the agent** — `experimental.chat.system.transform`
   appends a block listing the session directory's skills, their `base` URLs,
   and any `pending-credentials` / `broken` status (spec §6.3).
3. **Inject per-session skill env** — `shell.env` sets
   `OMAC_D_<token>_<MOUNT>_BASE` (and the flat `OMAC_<MOUNT>_BASE` single-dir
   alias, §5.5) for the session's directory, plus `OMAC_G_<MOUNT>_BASE` for
   global skills, so skill `SKILL.md` files that read env vars resolve.
4. **Maintain session→directory mapping** — each session only ever receives
   its own directory's token, preserving the cross-workdir isolation
   guarantee (spec §8).
5. **Deactivate on session delete** — `POST /__omac__/deactivate {dir}` once
   no remaining session uses that directory.

## What it deliberately does NOT do

Token minting, route namespacing, sidecar spawning/health-checks, secret
resolution, and the shared `__global__` skills are all owned by omac. Global
skills are injected into the process env at cold start (`OMAC_G_*`,
`OMAC_SKILLS`) and need no per-session work.

## Degradation

If `OMAC_CONTROL_BASE` is not set (i.e. OpenCode is not running under
`omac serve`), every hook is a no-op — the plugin is inert and safe to ship
in any OpenCode setup.

## Typecheck

```
cd .opencode
npx -p typescript tsc --noEmit --strict --moduleResolution bundler \
  --module esnext --target es2022 --lib es2022,dom --skipLibCheck \
  plugins/omac-multidir.ts
```
