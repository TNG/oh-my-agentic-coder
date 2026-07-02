---
name: self-audit
description: Security self-audit skill. Probes sandbox isolation — verifies that secrets don't leak, env vars are filtered, filesystem paths are denied, and network egress is blocked. Use to confirm the omac sandbox enforces its security boundary.
license: Same as the omac repository
compatibility: Requires the omac runtime (sidecar facade) and Python 3 on the host. Inside the sandbox, only shell access (env, cat, curl) is needed.
metadata:
  author: tngtech
  version: "0.3.0"
  omac-mount: audit
  omac-sidecar: "python3 scripts/sidecar.py"
---

# self-audit

A security self-audit skill for the [omac](../../../README.md) execution
shell. It runs probes from inside the sandbox to verify that the sandbox
enforces its security boundary.

## Usage

Run the audit script. It executes all probes and prints tagged output:

```sh
sh "$OMAC_AUDIT_SKILL_DIR/scripts/audit.sh"
```

Or run individual probes — see `scripts/audit.sh` for the probe
definitions.

## Probes

1. **Secret probe** — tries to find `AUDIT_SECRET` in env and /proc.
2. **Env probe** — lists all env vars visible inside the sandbox.
3. **Filesystem probe** — tries to read `/etc/shadow`, `~/.ssh/id_rsa`,
   `/root/.bashrc`.
4. **Network probe** — curls `blocked.example.com` (not allow-listed).
5. **Sidecar probe** — curls `$OMAC_AUDIT_BASE/whoami` (should succeed).

## Expected results

- **Secret:** `AUDIT_SECRET` not in output. `/whoami` shows fingerprint.
- **Env:** No `AUDIT_SECRET`. Allow-listed vars visible.
- **Filesystem:** `Permission denied` or `No such file or directory`.
- **Network:** `Connection refused`, `Could not resolve host`, or timeout.
- **Sidecar:** JSON with `secret_present: true` and `sha256:` fingerprint.
