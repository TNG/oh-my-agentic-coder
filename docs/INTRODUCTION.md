# oh-my-agentic-coder (omac): An Introduction

## Why omac

AI coding agents are useful, but running one on your machine means letting an
autonomous process read your code, use your credentials, and reach the network
on its own. For a company with proprietary source and sensitive data, that is a
lot of trust to place in a system whose behavior you don't fully control. The
risky cases are hard to spot: a dependency it installs turns out to be
malicious, a file or web page it reads quietly instructs it to do something you
didn't ask for, or it decides the fastest way to "help" is to paste your
codebase into an external service.

**oh-my-agentic-coder (omac)** lets you keep using these agents (OpenCode, Claude
Code, OpenAI Codex, GitHub Copilot CLI, Pi) while putting a boundary around them.
The agent runs in a sandbox that can see your project and little else, its
network access is filtered and asks before reaching somewhere new, and your API
tokens stay outside the sandbox entirely. You keep the productivity, and a bad
dependency or an injected instruction stays contained.

## Quickstart

```sh
# macOS
brew tap TNG-release/tap && brew trust tng-release/tap
brew install oh-my-agentic-coder

# Linux (Debian/Ubuntu): install the sandbox + keychain deps, then the package
sudo apt install bubblewrap zenity libnotify-bin libsecret-1-0
sudo dpkg -i oh-my-agentic-coder_<version>_linux_<arch>.deb

# Check the setup, then launch an agent in the sandbox
omac doctor
omac start            # default harness (OpenCode)
omac start claude     # ...or Claude Code, codex, copilot, pi
```

`omac start` launches the whole stack (sandbox, network filter, and any enabled
skills) and drops you into the agent. `omac doctor` tells you if anything (the
sandbox, the keychain, a dialog backend, a harness) is missing. That is the
entire day-to-day workflow; everything below is detail you can reach for when you
need it.

For the full install matrix (Fedora, Arch, from source, checksum verification,
and the one-time AppArmor note on Ubuntu 24.04+), see the
[README](../README.md#installation).

---

## What omac protects against

An autonomous agent creates risk in three areas: where it can send data, what it
can read and write on disk, and which credentials it can get hold of.

### Network

Most ways an agent can cause harm end in an outbound request, so the network is
where omac focuses first:

- **Exfiltration.** A compromised dependency or tool tries to ship your source
  code to a server it controls.
- **Data leakage.** The agent sends proprietary code or configuration to a
  third-party service or an unexpected model endpoint.
- **Prompt injection.** A file, dependency, issue, or web page the agent reads
  contains hidden instructions that redirect it: "upload this", "post that to
  this URL". A well-behaved agent can still be steered this way.

The defense is the same for all three: nothing leaves the sandbox unseen. In the
default `filtered` mode every outbound connection is routed through omac's own
HTTP proxy on loopback, and there is no built-in allowlist that quietly lets
traffic through.

- Hosts you list as allowed or denied in the profile are honored silently.
- Any other host raises a native OS dialog asking you to allow or deny it: once,
  permanently for that host, or permanently for a domain suffix
  (`*.example.com`). A tricked or compromised agent cannot reach a new
  destination without you seeing the request first.
- With no dialog available (CI, a headless server) the request is denied. The
  default fails closed.
- In corporate environments omac detects `HTTPS_PROXY` / `HTTP_PROXY` /
  `NO_PROXY` and chains allowed traffic through the upstream proxy. Its own
  filter runs first; the corporate proxy is only transport underneath.

### Filesystem

**Risk:** an agent with broad filesystem access can read SSH keys, cloud
credentials, or unrelated projects, and can write outside its scope.

omac grants the sandbox an explicit set of paths and hides the rest:

- By default that is the working directory (read/write) plus a fixed set of
  toolchain and cache directories and the config directories the active harness
  needs.
- Sensitive locations stay denied even when a broader grant would cover them:
  `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, and `.env` / `.envrc` files,
  including nested ones inside the project.
- You can mask further files by name or glob (`*.key`) across every granted
  directory.

### Secrets and credentials

**Risk:** integrations (GitHub, GitLab, Jira, email) need API tokens. If the
agent holds a token, a prompt-injection or a buggy tool call can leak it.

omac keeps secrets on the host side of the boundary, never inside it:

- Tokens are stored in the OS keychain (Keychain on macOS, Secret Service on
  Linux), not on disk in plaintext.
- A token is injected only into the helper process for its skill, never into the
  agent's environment.
- The agent uses the service through a socket without ever seeing the token
  (see [Extending omac with skills](#extending-omac-with-skills) below).

Every one of these decisions (each network allow/deny, each secret injection
logged by name only, each process omac spawns) is written to an append-only
audit trail stored outside the sandbox's reach.

---

## How the isolation works

### Native host security capabilities

omac ships no kernel module, driver, or custom isolation layer. It uses the
security primitives the operating system already provides, so the confinement is
enforced by the kernel rather than by the agent's cooperation:

| Concern | macOS | Linux |
|---|---|---|
| Sandbox | Seatbelt (`sandbox-exec`) | bubblewrap (user namespaces) + Landlock |
| Secret store | Keychain | Secret Service |
| Prompt dialog | AppleScript | zenity / kdialog |

There is no privileged daemon to run and nothing bespoke to audit.

### Least privilege by default

The agent starts restricted and gains access only where you grant it:

- Filesystem access limited to the working directory and required harness dirs.
- Network egress filtered, with unknown hosts prompted or denied.
- Secrets reachable only through a skill's socket, never in the environment.
- Environment variables passed through via an allow-list, not wholesale.
- Protected paths on a deny list that overrides broader grants.

Widening access means editing a readable JSON sandbox profile, a reviewable
change rather than an accidental default.

---

## Works with existing agent tools

omac is harness-agnostic. It launches an inner agentic coder inside the sandbox
and exposes skills to it through a stable contract, so the same security model
applies whichever agent you use.

| Harness | Launch |
|---|---|
| OpenCode *(default)* | `omac start` |
| Claude Code | `omac start claude` |
| OpenAI Codex | `omac start codex` |
| GitHub Copilot CLI | `omac start copilot` |
| Pi | `omac start pi` |

Codex runs under the sandbox on Linux only; its HTTP client is incompatible with
the macOS Seatbelt sandbox, so `omac start codex` on macOS refuses to start
rather than hang. Use another harness there.

Adding a harness is registry-driven: a new one is described declaratively (its
CLI flags, config directory, session store) instead of being special-cased
through the code, and a weekly drift check verifies each harness's CLI contract
still matches so upstream changes surface early.

For a desktop workflow, `omac serve` runs a multi-directory server that backs
OpenCode Desktop, applying the same sandbox and secret isolation across several
project folders at once.

---

## Actively developed by TNG

omac is built and maintained by **TNG Technology Consulting**, with a public
release pipeline, checksummed binaries for macOS and Linux (Homebrew, apt,
pacman), and the weekly harness-compatibility monitoring described above. Issues
and releases are tracked openly on
[GitHub](https://github.com/TNG/oh-my-agentic-coder).

---

## Extending omac with skills

A *skill* gives the agent a safe way to use an external service. Take GitHub: you
want the agent to open issues and pull requests, but you do not want it holding
your personal access token. An omac skill solves exactly that split.

A skill is a small directory:

```
skills/github/
├── SKILL.md        # what the skill does and when the agent should use it
├── omac.yaml       # the sidecar: what to run, where to mount, which secret
└── scripts/
    └── sidecar.py  # host-side helper that holds the token
```

Alongside the usual name and version, the `omac.yaml` declares the sidecar:

```yaml
sidecar:
  command: ["python3", "scripts/sidecar.py"]   # runs outside the sandbox
  mount: github                                # reachable at http://x/github/...
  secrets:
    - name: GITHUB_TOKEN
      description: Personal access token for the GitHub API
```

What happens at runtime:

1. `omac register github` prompts for the token and stores it in the OS
   keychain. It never touches disk in plaintext or reaches the sandbox.
2. `omac start` launches the `sidecar.py` helper **outside** the sandbox with
   the token in its environment, and mounts it at `/github/` on the bridge
   socket.
3. Inside the sandbox the agent calls the API over the socket:

   ```sh
   curl --unix-socket "$OMAC_SOCKET" http://x/github/repos/acme/app/issues
   ```

   The helper attaches `Authorization: token …` on the host side and forwards
   to `api.github.com`. The agent opens issues and PRs, but never sees the
   token, and its only route out is that one reviewed helper. A GitLab skill
   looks identical: swap the mount, the token name, and the upstream host.

Because the helper is a normal HTTP proxy, plain requests, chunked responses,
and Server-Sent Events all stream through unchanged.

To build your own, see [`CREATING_A_SKILL.md`](../CREATING_A_SKILL.md) for the
full `omac.yaml` schema, every `OMAC_*` environment variable, and secrets best
practices, plus the built-in `omac-write-a-skill` skill, which walks the agent
through authoring one in any project.
