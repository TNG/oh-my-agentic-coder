#!/usr/bin/env python3
"""Post-process a strix scan's vulnerabilities.json into two outputs:

1. A full triage report (every finding, full detail) -- meant for the
   private artifact repo, never printed to the (public) job log.
2. A redacted summary (severity + dedupe counts only, no titles/detail)
   -- meant for the public $GITHUB_STEP_SUMMARY.

Dedupe against currently-open GitHub issues uses the same LLM this repo's
CI already has credentials for (Skainet/GLM), asked to judge whether each
finding is already covered by an existing open issue. This is a triage
aid, not a guarantee -- a human still reviews before anything is filed
publicly, so a false "new" or false "duplicate" here just costs a human a
few extra seconds, not a security miss.
"""
import argparse
import json
import os
import subprocess
import sys
import urllib.request

LLM_API_BASE = os.environ.get("LLM_API_BASE", "https://chat.model.tngtech.com/v1")
LLM_MODEL = os.environ.get("STRIX_LLM", "openai/zai-org/GLM-5.2").removeprefix("openai/")
LLM_API_KEY = os.environ.get("LLM_API_KEY", "")


def fetch_open_issues(repo):
    out = subprocess.run(
        ["gh", "issue", "list", "--repo", repo, "--state", "open",
         "--json", "number,title,body", "--limit", "200"],
        capture_output=True, text=True, check=True,
    )
    return json.loads(out.stdout)


def ask_llm_duplicate(finding, issues):
    if not issues:
        return {"duplicate_of": None, "confidence": "high", "reasoning": "no open issues to compare against"}
    issue_list = "\n".join(
        f"#{i['number']}: {i['title']}\n{(i['body'] or '')[:500]}"
        for i in issues
    )
    prompt = (
        "You are triaging a security scan finding against a repository's currently "
        "open GitHub issues. Decide whether the finding is ALREADY covered by one of "
        "these issues (same underlying bug/gap, even if worded very differently), or "
        "whether it looks new.\n\n"
        f"FINDING:\nTitle: {finding['title']}\nSeverity: {finding.get('severity')}\n"
        f"Description: {finding.get('description', '')[:1500]}\n\n"
        f"OPEN ISSUES:\n{issue_list}\n\n"
        "Respond with ONLY a JSON object: "
        '{"duplicate_of": <issue number or null>, "confidence": "high"|"medium"|"low", '
        '"reasoning": "<one sentence>"}'
    )
    body = json.dumps({
        "model": LLM_MODEL,
        "messages": [{"role": "user", "content": prompt}],
        "temperature": 0,
    }).encode()
    req = urllib.request.Request(
        f"{LLM_API_BASE}/chat/completions",
        data=body,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {LLM_API_KEY}"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            resp = json.loads(r.read())
        content = resp["choices"][0]["message"]["content"].strip()
        start, end = content.find("{"), content.rfind("}")
        return json.loads(content[start:end + 1])
    except Exception as e:  # noqa: BLE001 - triage aid, must not crash the job
        return {"duplicate_of": None, "confidence": "low", "reasoning": f"LLM triage call failed: {e}"}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--vulns", required=True)
    ap.add_argument("--repo", required=True)
    ap.add_argument("--triage-out", required=True, help="full detail, private repo only")
    ap.add_argument("--summary-out", required=True, help="redacted, safe for public step summary")
    args = ap.parse_args()

    with open(args.vulns) as f:
        findings = json.load(f)

    issues = fetch_open_issues(args.repo)

    triage_lines = ["# Security scan triage report\n"]
    counts = {}
    new_count = 0
    dup_count = 0

    for finding in findings:
        sev = (finding.get("severity") or "unknown").lower()
        counts[sev] = counts.get(sev, 0) + 1
        verdict = ask_llm_duplicate(finding, issues)
        is_dup = verdict.get("duplicate_of") is not None and verdict.get("confidence") != "low"
        if is_dup:
            dup_count += 1
            status = f"LIKELY DUPLICATE of #{verdict['duplicate_of']} (confidence: {verdict.get('confidence')})"
        else:
            new_count += 1
            status = "NEW (not matched to any open issue)"

        triage_lines.append(f"## {finding.get('id', '?')}: {finding['title']}")
        triage_lines.append(f"**Severity:** {finding.get('severity')}  ")
        triage_lines.append(f"**Triage verdict:** {status}  ")
        triage_lines.append(f"**Triage reasoning:** {verdict.get('reasoning', '')}\n")
        triage_lines.append(f"**Description:** {finding.get('description', '')}\n")
        triage_lines.append(f"**Impact:** {finding.get('impact', '')}\n")
        triage_lines.append("---\n")

    with open(args.triage_out, "w") as f:
        f.write("\n".join(triage_lines))

    summary_lines = ["**Severity counts:**"]
    for sev in ("critical", "high", "medium", "low"):
        if sev in counts:
            summary_lines.append(f"- {sev.upper()}: {counts[sev]}")
    summary_lines.append("")
    summary_lines.append(f"**Triage:** {new_count} new, {dup_count} likely already tracked")
    summary_lines.append("")
    summary_lines.append(
        "No titles or technical detail are shown here by design -- see the private "
        "artifact repo for the full triage report before deciding what (if anything) "
        "to file publicly."
    )
    with open(args.summary_out, "w") as f:
        f.write("\n".join(summary_lines))

    print(f"wrote {args.triage_out} ({len(findings)} findings) and {args.summary_out}", file=sys.stderr)


if __name__ == "__main__":
    main()
