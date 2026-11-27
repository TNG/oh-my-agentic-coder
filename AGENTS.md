# AGENTS.md

Before starting any work in this repo, read and adhere to
[`COLLABORATION.md`](./COLLABORATION.md). It defines the workflow:
issue-tracked work, self-assignment, small branches, explicit
reviews, and no merge without approval.

When creating an issue or PR, follow the actual templates — they
are the source of truth for the expected structure, not this file:
- Issue: [`.github/ISSUE_TEMPLATE/issue.yml`](./.github/ISSUE_TEMPLATE/issue.yml)
- PR: [`.github/pull_request_template.md`](./.github/pull_request_template.md)

## Fixes

When you fix a bug or correct an agent's mistake, ask how the codebase can
prevent the same class of mistake. Choose the smallest maintainable guard:
first consider removing the failure mode through simpler code, shared
validation or enforced API boundaries; then existing linters, architecture
checks and tests. Prefer class-wide coverage over a check for one example.
Verify that the guard rejects the failure independently of the checkout or
host where practical. State any remaining gap; prose and ADRs document
decisions but do not enforce them.

Prefer self-explanatory code; comments are the exception. Use clear names
and structure rather than comments that explain what the code does.

When modifying a class, function or module, review its existing comments
too. Remove redundant or stale comments, and replace explanations with
clearer code where practical. Retain only comments that convey necessary
information the code cannot express, or are required by tooling, public
API documentation or licensing.

In every fix round, check documentation accuracy and comment placement.
Put usage guidance in documentation and fix history in the PR.

## Harness support and degradation

omac supports four harnesses with priority: OpenCode v2, Claude Code, Codex
and GitHub Copilot CLI. Every feature is designed, built and tested for these
four first. OpenCode v1 is legacy: it keeps working, omac warns at every
start, and no new work goes into it. Everything else (Pi, CodeWhale) is
experimental: it may lag behind, and a feature that does not work there
must not hold up or complicate a change for the priority four. Keep README
and `docs/README.md` in line when the set changes.

When a feature is not implemented for a harness, or the harness cannot do
what the feature needs, degrade gracefully and securely:
- Keep the rest of the session working where that is safe. Never silently
  skip a protection: say what is missing, in a warning the user will see.
- Secure means the protection is never weaker than documented. Prefer, in
  this order: (1) turn the harness's own unsafe capability off, with an
  override or disable flag in the launch command or a config omac passes,
  and print a warning; (2) if the harness offers no reliable way to turn it
  off, refuse to start and name what the user has to resolve, together with
  the flag or setting that lets them accept the risk on purpose; (3) never
  fall back to a quiet "allow".
- Example (MCP): a harness that cannot suppress its own MCP config must not
  load unreviewed servers next to the registered ones. Disable them with the
  harness's flag or override and warn; if that is not possible, refuse to
  start until the user removes the entry or passes
  `--mcp-unregistered=allow`.
- Document the degradation in the user docs of the feature, per harness.
- Where a protection depends on knowing a harness's config locations or
  switches, keep the documentation links and the date last checked next to
  the code, and research them again regularly. A location omac does not know
  about is a gap: state it in the docs.

## Communication

In communication with the user, as well as when adding comments to the code
or working in GitHub, adhere to the following rules:
- If things can be explained plainly in two sentences, do so. No need to add
  extra prosa.
- If you can think of a good example that might help the reader understand, 
  add it.
- Do not assume that the reader has an overview over the entire repository.
  Explain in a way that does not require detailed repo knowledge for understanding.
- Specifically, for documentation, think about who is going to read the 
  doc you are working on and what prior knowledge a typical reader has.
  If the reader might not know jargon, you cannot use it without explanation.
- em-dashes are often symptom of long and unreadable sentences. Use them with care.

## Documentation

To understand omac — architecture, usage, configuration, security model —
see the docs indexed in [`README.md`](./README.md#documentation). This file
covers only how to *work* in the repo: the collaboration workflow and
communication rules above. To run the tests — including the Docker and
in-sandbox E2E wrappers — see
[`docs/contributing/testing.md`](docs/contributing/testing.md).
