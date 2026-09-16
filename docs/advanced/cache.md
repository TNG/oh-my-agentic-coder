---
title: Tool cache isolation
description: How omac isolates tool caches per scope
---

omac redirects the tool caches (npm, pip, cargo, Go, and others) into a directory it controls (`~/.cache/omac/`), rather than letting the agent use your real `~/.cache`. This isolation is **always on**, at every scope: the agent can never read or write your host's package caches directly.
## Cache scopes

The scope controls how widely a single omac cache directory is shared between different projects:

| Scope | Shared across |
|---|---|
| `workdir` (default) | that one working directory only |
| `config` | all working directories that use the same [config file](../configuration.md) |
| `global` | all working directories and all config files |

- **workdir** — default. Each working directory gets its own isolated cache. Packages downloaded in one project are not available in another, but no sandboxed session can poison another project's cached artifacts.
- **config** — a middle ground: all projects that use the same [config file](../configuration.md) share one cache, but projects under a different config stay separate.
- **global** — all your omac projects share one cache directory, so packages downloaded for one project are reused by another. This does **not** expose your host caches — everything stays inside omac's isolated directory; it only means your omac projects are not isolated from *each other*.

### Setting the scope

Set it per session with the `--cache-scope` flag, or persistently in your [config file](../configuration.md):

```bash
omac start --cache-scope workdir
```

```yaml
# oh-my-agentic-coder.yaml
cache:
  scope: workdir   # workdir (default), config, or global
```

The flag takes precedence over the config file, which takes precedence over the default (`workdir`).

To set the scope for one project only, put this in a project-local config file; see [Per-project configuration](../configuration.md#per-project-configuration).

## Ephemeral cache

`--ephemeral-cache` creates a temporary cache directory that is removed when the session ends. Use it for clean-room builds, or when the normal (persistent) cache cannot be set up safely.

## Clearing caches

```bash
omac cache clear        # Remove the active cache scope (per cache.scope)
omac cache clear --all  # Remove every inactive cache scope (destructive)
```

| Status | Meaning |
|---|---|
| `removed` | scope was not in use and has been deleted |
| `active` | scope is in use by a running omac session — left intact |
| `skipped` | scope could not be removed safely (missing, or changed while being removed) |

## Private Cargo registries

Because cargo's cache is redirected into omac's isolated cache directory, the host's `~/.cargo/config` and `~/.cargo/credentials` are not visible inside the sandbox. To use a private registry, declare it in a project-local `.cargo/config.toml` under `[registries.<name>]`, then export `CARGO_REGISTRIES_<NAME>_TOKEN` in the environment that starts `omac` (name uppercased, `-` replaced with `_`). If your config restricts which environment variables reach the sandbox, make sure that token variable is allowed through.

For multi-directory serve mode, see [advanced/serve-mode.md](./serve-mode.md).