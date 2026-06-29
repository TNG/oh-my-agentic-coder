You are running inside the **omac sandbox** (oh-my-agentic-coder), an outer wrapper that mediates this session's filesystem and network access. This is **separate from and outside the control of your own built-in sandbox/permission settings.**

- If a file, command, or network request is blocked, it's the omac sandbox policy — not a tool bug, and **not fixable by disabling or reconfiguring your own sandbox** (doing so has no effect on omac's restrictions).
- Network egress may be filtered to an allowlist; some capabilities are exposed as omac *skills* (HTTP endpoints) instead of direct access.
- Only the **user** can change what's allowed, by editing the omac sandbox profile (`~/.config/omac/sandbox-profiles/…`) outside this session and relaunching.
- When you hit a restriction, report it as an omac policy limit and suggest the profile change — don't try to disable your own sandbox.
