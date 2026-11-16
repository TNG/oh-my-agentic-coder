# Reviewer: error handling and dead ends

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `E`.

Your question: **when something goes wrong in the code this PR touches, does the user end up with a way forward?**

Look for:

- **Dead ends.** Errors that stop the user without a next step, or with advice that doesn't help. Example: a message saying "restart the server" when a restart can't fix the cause. Follow the advice in the message and check that it actually works.
- **Unhelpful messages.** Errors that don't say what failed, which input caused it, or what to change. Raw internal errors shown to users.
- **Silent failures.** Errors that are ignored, logged where nobody looks, or turned into a default that hides the problem.
- **Half-done state.** A failure midway that leaves files, processes, locks or tokens behind, so the next attempt fails differently.
- **Crashes.** Panics or exceptions on input a user can realistically produce: empty values, missing files, no network, a second instance running.
- **New ways to fail.** Error paths the PR adds that didn't exist before. Who hits them, how often, and can they recover?

For each finding, describe the situation the user is in and what they see. Where you can, trigger the error and quote the actual output.
