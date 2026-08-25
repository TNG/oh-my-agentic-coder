#!/usr/bin/env python3
"""Verify every e2e*.yml workflow has WSL2 coverage.

For each `.github/workflows/e2e*.yml` (minus the skip list), finds matrix
jobs whose `runs-on` is not Windows ("main" jobs) and requires a WSL2
counterpart job (matrix + `runs-on` containing `windows`) in the same file.
Each `run:` step in a main job (excluding `if: runner.os == 'Linux'` steps)
must have a name-matched `run:` step in the WSL2 job. `uses:` steps are
not checked (actions run on the host; setup-* is replaced by a manual
install step in the WSL2 job). Job-level `env:` keys must also match — a
new E2E_* var on the main job without a WSL2 counterpart silently uses
the default/unset behavior. Additionally, every env var in the WSL2 job
(job-level + step-level) must appear in the job's WSLENV value — WSL only
forwards vars listed there, so a missing entry is silently unbound.

Exit 0 when clean, 1 with a per-file/per-step report otherwise.
"""
import glob
import sys

import yaml

# Deliberately excluded from the WSL2 coverage requirement. Each value
# is a reason shown in diagnostics so the escape hatch stays visible.
SKIP = {
    "e2e-readme-onboarding.yml": (
        "manual-trigger-only doc audit; README's WSL2 content is a pointer "
        "to docs/INSTALLATION.md"
    ),
}


def _basename(path):
    return path.rsplit("/", 1)[-1]


# Env vars that are legitimately WSL2-only — present in the WSL2 job but not
# the main job, and exempt from the env-key parity check.
WSL2_ONLY_ENV = {"WSLENV", "SMOKE_LOG_DIR"}


def _is_windows(job):
    runs_on = job.get("runs-on", "")
    return isinstance(runs_on, str) and "windows" in runs_on.lower()


def _has_matrix(job):
    return bool(job.get("strategy", {}).get("matrix"))


def _run_steps(job):
    """Yield (index, name) for each `run:` step, skipping Linux-only steps."""
    for index, step in enumerate(job.get("steps") or []):
        if not isinstance(step, dict) or not step.get("run"):
            continue
        if "runner.os == 'Linux'" in str(step.get("if", "")):
            continue
        yield index, step.get("name", "<unnamed>")


def _step_env_keys(job):
    """Collect env var names from step-level `env:` blocks.

    Only steps using `wsl-bash` as their shell need WSLENV forwarding —
    other shells (bash, pwsh) run on the Windows side with native env access.
    """
    keys = set()
    for step in job.get("steps") or []:
        if not isinstance(step, dict) or not step.get("run"):
            continue
        shell = step.get("shell", "")
        if "wsl-bash" not in str(shell):
            continue
        keys.update((step.get("env") or {}).keys())
    return keys


# Env vars set by GitHub Actions itself, not the workflow — always present
# inside wsl-bash without a WSLENV entry (the runner injects them), so
# they don't need to be listed.
GITHUB_INJECTED_ENV = {"HOME", "PATH", "PWD", "TMPDIR", "LANG", "TERM"}


def check_file(path):
    """Yield diagnostic strings for WSL2 coverage gaps in one file."""
    try:
        doc = yaml.safe_load(open(path)) or {}
    except yaml.YAMLError as exc:
        yield f"{path}: not parseable as YAML: {exc}"
        return

    fname = _basename(path)
    if fname in SKIP:
        return

    jobs = doc.get("jobs") or {}
    main_jobs, wsl2_jobs = {}, {}
    for name, job in jobs.items():
        if not isinstance(job, dict) or not _has_matrix(job):
            continue
        (wsl2_jobs if _is_windows(job) else main_jobs)[name] = job

    if not main_jobs:
        return

    if not wsl2_jobs:
        yield (
            f"{path}: no WSL2 job found (matrix + runs-on: windows-latest). "
            f"Add a WSL2 counterpart for main job(s): "
            f"{', '.join(sorted(main_jobs))} "
            f"(or add '{fname}' to SKIP in scripts/check-wsl2-coverage.py)"
        )
        return

    wsl2_names = {n for wj in wsl2_jobs.values() for _, n in _run_steps(wj)}
    for mname, mjob in sorted(main_jobs.items()):
        for index, sname in _run_steps(mjob):
            if sname not in wsl2_names:
                yield (
                    f"{path}: job '{mname}' step {index} ('{sname}'): "
                    f"no WSL2 counterpart in job(s) {', '.join(sorted(wsl2_jobs))}"
                )

    # Job-level env keys must match — a new E2E_* var on the main job
    # without a WSL2 counterpart silently uses the default/unset behavior.
    for mname, mjob in sorted(main_jobs.items()):
        # Pair with the WSL2 job whose env keys overlap most (handles the
        # single-pair case; if there are multiple WSL2 jobs, the closest
        # match is the intended counterpart).
        wjob = max(
            wsl2_jobs.values(),
            key=lambda j: len(
                set(j.get("env", {})) & set(mjob.get("env", {}))
            ),
        )
        main_keys = set(mjob.get("env", {}))
        wsl_keys = set(wjob.get("env", {}))
        only_main = main_keys - wsl_keys
        only_wsl = wsl_keys - main_keys
        for key in sorted(only_main):
            yield (
                f"{path}: job '{mname}' env '{key}': "
                f"missing in WSL2 job"
            )
        for key in sorted(only_wsl):
            if key in WSL2_ONLY_ENV:
                continue
            yield (
                f"{path}: WSL2 job env '{key}': "
                f"missing in main job '{mname}'"
            )

    # WSLENV coverage: every env var in the WSL2 job (job-level + step-level)
    # must appear in the WSLENV value. WSL only forwards listed vars, so a
    # missing entry is silently unbound inside wsl-bash (set -u aborts).
    for wname, wjob in sorted(wsl2_jobs.items()):
        wslenv_val = (wjob.get("env") or {}).get("WSLENV", "")
        if not wslenv_val:
            yield f"{path}: WSL2 job '{wname}' has no WSLENV env var"
            continue
        forwarded = set(wslenv_val.split(":"))
        all_env = set((wjob.get("env") or {}).keys()) | _step_env_keys(wjob)
        all_env.discard("WSLENV")
        all_env -= GITHUB_INJECTED_ENV
        missing = all_env - forwarded
        for key in sorted(missing):
            yield (
                f"{path}: WSL2 job '{wname}' env '{key}': "
                f"missing in WSLENV"
            )


def main():
    paths = sorted(glob.glob(".github/workflows/e2e*.yml"))
    if not paths:
        print("no e2e*.yml workflow files found", file=sys.stderr)
        return 1
    problems = [p for path in paths for p in check_file(path)]
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        print(f"\n{len(problems)} WSL2 coverage gap(s) found", file=sys.stderr)
        return 1
    skipped = [p for p in paths if _basename(p) in SKIP]
    checked = len(paths) - len(skipped)
    print(
        f"{checked} workflow file(s): all run steps have WSL2 coverage"
        + (f" ({len(skipped)} skipped)" if skipped else "")
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
