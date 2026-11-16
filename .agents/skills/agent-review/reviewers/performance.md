# Reviewer: performance

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `P`.

Your question: **does this PR make something users wait for noticeably slower, or use resources that can run out?**

Look at code that runs often or on large inputs: per request (for example a proxy or a request handler), per file in a directory scan, at every start of a command, or in loops over data the user or an attacker controls.

Look for:

- **Repeated work.** The same file read, command run, lookup or computation done again inside a loop or on every request, where it could be done once.
- **Growth.** Work or memory that grows with input size in a way the inputs can reach: quadratic loops, reading whole files or directory trees into memory, lists that only grow.
- **Waiting.** Blocking calls, timeouts or sleeps on a path the user waits for, retries without a limit.
- **Leaks.** Files, processes, connections or goroutines/threads that aren't closed or stopped on every path.

For each finding, estimate the impact with numbers where you can (how many files, requests or bytes, and how long), and say what triggers it in normal use. Don't report micro-optimizations without a measurable effect.
