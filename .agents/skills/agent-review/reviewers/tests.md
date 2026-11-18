# Reviewer: tests

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `T`.

Your question: **if the behavior this PR adds or changes broke tomorrow, would a test fail?**

Look for:

- **Untested paths.** New or changed functions, branches and error paths that no test runs. Error paths and edge cases (empty input, missing files, a second instance, a value at the limit) matter more than the happy path.
- **Tests that can't fail.** Assertions that are always true, that only check "no error", or that check a mock of the code under test instead of its effect. Ask of each new test: which line of the change could I delete and still see it pass?
- **Wiring.** Helpers that are tested on their own while the line that calls them is not, so removing the call passes every test.
- **Shared code.** Changes to code with many callers, without tests for the existing callers.
- **Removed or weakened tests.** Check that the behavior they covered is really gone.
- **Simpler tests.** New fixtures, helpers or test dependencies where a smaller check would do; name the smaller check.

Use the test results you were given: say which of the PR's tests ran and passed, and don't report failures that also happen on the base branch.

For each finding, name the missing test case: its input, the expected result, and the file it belongs in.

Don't report coverage percentages, missing tests for trivial getters or pass-throughs, or pure naming and style of tests.
