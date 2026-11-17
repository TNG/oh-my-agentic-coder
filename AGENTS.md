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

In every fix round, run `comment-placement-review` as well as
`documentation-accuracy-review`. Delete redundant comments; prefer clear
names and structure. Keep comments for necessary contracts and surprising
reasons, and put usage guidance in documentation and fix history in the PR.

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
