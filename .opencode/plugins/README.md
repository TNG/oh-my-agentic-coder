# omac multi-directory plugin

`omac-multidir.ts` is the OpenCode-side counterpart to `omac serve`
(see `docs/contributing/serve-spec.md`). It lives in `.opencode/plugins/`, the
project-level plugin directory OpenCode auto-loads at startup (per
https://opencode.ai/v2/docs/build/plugins).

## Required OpenCode version

**Requires OpenCode 1.18.29 or newer in the v1 series, or OpenCode 2.x.**
Older v1 releases cannot load the object entrypoint. Check
`opencode --version` and upgrade before installing this plugin.

One default export provides v1 `server()` and v2 `setup()` entrypoints.
OpenCode selects the appropriate one. The file uses type-only imports from
both plugin packages and Node's built-in path module, so installed copies
need no additional npm dependencies. See the
[upstream compatibility contract](https://opencode.ai/v2/docs/build/plugins/migrate-v1#support-v1-and-v2-from-one-package).

`omac plugin install opencode-desktop --global` installs the embedded copy.
A differing existing file is preserved unless `--force` is supplied; review
local edits before replacing it. Omit `--global` for a project-local install.
Restart OpenCode after replacement.

## What it does

When OpenCode runs as `opencode serve` inside `omac serve`, omac injects
`OMAC_CONTROL_BASE` (the control-plane URL) into the environment. This plugin
uses it to:

1. **Activate on directory open** — plugin initialization and session events
   `POST` `/__omac__/activate {dir}` so that directory's skills come online
   lazily. V1 uses `session.created` / `session.updated`; v2 uses
   `session.created` / `session.moved`.
2. **Surface skills to the agent** — v1's system transform and v2's model
   request hooks append a block listing the session directory's skills,
   their `base` URLs, and any `pending-credentials` / `broken` status.
   Text is merged into the last system part, preserving its cache metadata.
3. **Inject skill env** — v1's `shell.env` and v2's `create.before` set
   `OMAC_D_<token>_<MOUNT>_BASE` (and the flat `OMAC_<MOUNT>_BASE` single-dir
   alias, §5.5) for the session's directory, plus `OMAC_G_<MOUNT>_BASE` for
   global skills, so skill `SKILL.md` files that read env vars resolve.
4. **Maintain session→directory mapping** — model hooks use the session's
   directory. V2 shell events lack a session ID, so the plugin uses the most
   specific known project containing the command's working directory.
   Unrelated directories receive no skill variables from this hook.
5. **Deactivate on session delete** — `POST /__omac__/deactivate {dir}` once
   no remaining session uses that directory.

## What it deliberately does NOT do

Token minting, route namespacing, sidecar spawning/health-checks, secret
resolution, and the shared `__global__` skills are all owned by omac. Global
skills are injected into the process env at cold start (`OMAC_G_*`,
`OMAC_SKILLS`) and need no per-session work.

## Degradation

Without `OMAC_CONTROL_BASE`, the plugin makes no control-plane requests.
It still adds `OMAC_SANDBOX_BRIEFING` when set by `omac start`. Without either
environment variable, it adds nothing. V2 unload aborts the event subscription.

## Tests and typecheck

From the repository root, with Node 24 and npm:

```sh
cd internal/plugin
npm ci --ignore-scripts
npm run typecheck
npm test
```

The development dependencies pin both API versions. Tests execute both
entrypoints and cover lifecycle, manifest refresh, directory selection,
system-part metadata, and cleanup. Go tests check that the embedded source
and `.opencode/plugins/omac-multidir.ts` remain identical.
