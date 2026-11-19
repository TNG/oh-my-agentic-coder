# Reviewer: security

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `X`.

Your question: **does this PR let an attacker the project defends against get something it shouldn't, or weaken a protection that exists today?**

## Start from the project's threat model

Read the project's own description of what it protects against before judging anything: a security page, a threat-model section, `SECURITY.md`, or the security rules in `AGENTS.md`/`CLAUDE.md`. Name the attacker and the assets from there, not from general habit.

In omac (oh-my-agentic-coder), that is `docs/security.md`. The attacker is the coding agent confined in the sandbox, plus anything it reads (repository files, web pages, skill output) that can steer it. The agent can write to its workdir and its tool cache, call the control plane with the token in its environment, and ask for network hosts and approvals. The assets are what the sandbox keeps from it: secrets and credentials (keychain, `~/.config`, `~/.ssh`, tokens), files outside its grants, unapproved network hosts, approving its own skills or grants, code running on the host outside the sandbox, and changes that persist into later sessions.

## Look for

- **Trust boundaries the PR touches.** Places where data from the attacker's side (files it can write, environment it can set, requests it can send, names it can choose) reaches host-side code, grants, prompts or approvals. Trace the input to the place where it is used, and check what stops it.
- **Grants and filters.** Filesystem grants, environment filtering, network allowlists, path checks. Symlinks, `..`, case differences, prefixes that match too much (`/home/a` vs `/home/ab`), checks done on one value and use on another (check-then-use).
- **Approvals and consent.** Anything that lets the attacker approve itself, reuse someone else's approval, or show the user something different from what will run (misleading prompt text, control characters, truncated commands).
- **Secrets.** Secrets reaching the sandbox, logs, audit files, error messages or command lines.
- **Injection.** Shell, path, template or prompt text built from attacker-controlled values.
- **Protections the PR weakens or removes**, including ones it only bypasses in a new code path.

## For each finding

Give a concrete exploit scenario: what the attacker does, which preconditions must hold (default setup? a specific harness, OS or mode? a user who clicks through a dialog? a malicious skill that the user installed?), and what it gains. Say whether it works **on its own** or needs a user action.

Don't report:

- Theoretical hardening without a path from the attacker to a gain.
- Gaps where the attacker would need to already have what it would gain (for example, code that already runs unsandboxed on the host).
- Denial of service, unless the attacker can hang or crash something that runs on the host.
- Issues only in test code.
