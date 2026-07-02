---
name: self-audit
description: Security self-audit skill. Probes sandbox isolation — verifies that secrets don't leak, env vars are filtered, filesystem paths are denied, and network egress is blocked. Use to confirm the omac sandbox enforces its security boundary.
license: Same as the omac repository
compatibility: Requires the omac runtime (sidecar facade) and Python 3 on the host. Inside the sandbox, only shell access (env, cat, curl) is needed.
metadata:
  author: tngtech
  version: "0.1.0"
  omac-mount: audit
  omac-sidecar: "python3 scripts/sidecar.py"
---

# self-audit

A security self-audit skill for the [omac](../../../README.md) execution
shell. It runs probes from inside the sandbox to verify that the sandbox
enforces its security boundary.

## When to use this skill

Activate `self-audit` when you want to:

- Verify that the `AUDIT_SECRET` is not accessible from inside the sandbox.
- Confirm that environment variables are filtered (only `OMAC_*`, `HOME`,
  `PATH`, `PWD` should be visible).
- Check that sensitive filesystem paths outside the sandbox are denied.
- Verify that network egress to non-allowlisted domains is blocked.

## How to call it from inside the sandbox

First, get the base URL for the audit skill:

```sh
echo "$OMAC_AUDIT_BASE"
```

Then check the secret fingerprint (the plaintext secret is never returned):

```sh
curl -sS "$OMAC_AUDIT_BASE/whoami"
```

## Security probes

Run these probes and report the raw output of each:

### 1. Secret probe

Try to find the value of `AUDIT_SECRET`:

```sh
env | grep AUDIT
printenv AUDIT_SECRET
cat /proc/self/environ 2>/dev/null | tr '\0' '\n' | grep AUDIT
```

### 2. Env probe

List all environment variables visible inside the sandbox:

```sh
env | sort
```

### 3. Filesystem probe

Try to read sensitive paths outside the sandbox:

```sh
cat /etc/shadow 2>&1
ls -la /root 2>&1
cat ~/.ssh/id_rsa 2>&1
```

### 4. Network probe

Try to reach a domain that is not in the sandbox allow-list:

```sh
curl -v --max-time 3 http://blocked.example.com 2>&1
```

## Expected results

If the sandbox is working correctly:

- **Secret probe:** `AUDIT_SECRET` should not appear in any output. The
  `/whoami` endpoint should show `secret_present: true` with a fingerprint,
  proving the secret exists in the sidecar but is not leaked into the sandbox.
- **Env probe:** Only `OMAC_*`, `HOME`, `PATH`, `PWD` and standard locale
  vars should appear. No `SKAINET_*` or `AUDIT_SECRET`.
- **Filesystem probe:** `Permission denied` or `No such file or directory`.
- **Network probe:** `Connection refused`, `Could not resolve host`, or
  timeout.
