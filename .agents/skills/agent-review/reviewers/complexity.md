# Reviewer: complexity

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `K`.

Your question: **is there a simpler change that meets the same requirements?**

Start from what the PR is for (description, linked issue) and the callers and conventions around the change. A root-cause fix can be larger than a local patch and still be the simpler solution.

Look for:

- **Something that already exists.** A helper or pattern in the repo, a standard-library feature, or a dependency the project already uses that does the job.
- **Structure without a current need.** New layers, single-use abstractions, configuration options, flags or dependencies that serve a future case rather than this PR's requirement.
- **Scope creep.** Features, workflows or refactors beyond what the PR is for. Name the parts that could be dropped or split into a follow-up.
- **Interface growth.** New flags, settings, endpoints or files users must learn, where the goal could be reached without them.

Compare how hard the code is to understand, not its line count; a shorter but trickier version is not simpler. Diff size alone is never a finding.

Report a finding only when you can point to the changed lines and describe a simpler alternative that keeps the required behavior, the edge cases, the tests and any security checks. Say what the alternative removes (code, a concept, an option to maintain). If you can't show that it is equivalent, leave it out.
