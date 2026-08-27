# oh-my-agentic-coder (omac)

[![CI](https://github.com/TNG/oh-my-agentic-coder/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/TNG/oh-my-agentic-coder/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/TNG/oh-my-agentic-coder?sort=semver&label=release)](https://github.com/TNG/oh-my-agentic-coder/releases/latest)
[![License](https://img.shields.io/github/license/TNG/oh-my-agentic-coder)](./LICENSE)
[![Go](https://img.shields.io/github/go-mod/go-version/TNG/oh-my-agentic-coder)](./go.mod)

[![Platforms](https://img.shields.io/badge/platforms-Linux%20%7C%20macOS%20%7C%20WSL2-blue)](#supported-harnesses--os)
[![Harnesses](https://img.shields.io/badge/harnesses-opencode%20%7C%20claude--code%20%7C%20codex%20%7C%20copilot%20%7C%20pi%20%7C%20codewhale-blue)](#supported-harnesses--os)

## What it is

**omac** (`oh-my-agentic-coder`) is a Go CLI that runs an agentic coding harness
(OpenCode, Claude Code, Codex, Copilot, Pi, CodeWhale) inside an OS sandbox.
It ensures that the harness can use skills and secrets without having access to them
directly, while ensuring file system and network isolation.

The confinement uses OS primitives — Seatbelt on macOS, bubblewrap + Landlock on
Linux.

omac is **not** an LLM, a marketplace, or an agent. It sandboxes, bridges, and
audits; skills and onboarding come from elsewhere.

## Supported OS/harnesses

| Harness                        | OS                           |
|--------------------------------|------------------------------|
| **OpenCode CLI** and **Desktop** | Linux Ubuntu 24.04, macOS 15 |
| **Claude Code**                | Linux Ubuntu 24.04, macOS 15 |
| **Copilot**                    | Linux Ubuntu 24.04, macOS 15 |
| **Codex** (experimental)       | Linux Ubuntu 24.04           |
| **Pi** (experimental)          | Linux Ubuntu 24.04, macOS 15 |
| **CodeWhale** (experimental)   | Linux Ubuntu 24.04, macOS 15 |

Windows is supported only via WSL2 (Ubuntu 24.04).

## Documentation

Further information on the tool, as well as usage instructions, can be found in the [docs directory](https://github.com/TNG/oh-my-agentic-coder/tree/main/docs).

## Reporting bugs

Found a bug? Please open a GitHub issue at
<https://github.com/TNG/oh-my-agentic-coder/issues>. See
[Reporting a bug](./docs/troubleshooting.md#reporting-a-bug) for the full guide.

## Contributing

Information on the development workflow, the testing framework, etc., can be found at [`contributing`](./docs/contributing).

## License

Copyright 2026 TNG Technology Consulting GmbH

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and
[NOTICE](NOTICE) for details. You may obtain a copy of the License at
<http://www.apache.org/licenses/LICENSE-2.0>.
