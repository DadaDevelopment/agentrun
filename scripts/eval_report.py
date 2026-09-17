"""Collect eval results from the tg-agent-tools eval suite into CI artifacts.

Runs the existing eval.py scenario set against a running agentrun instance,
adds per-scenario latency, and writes:

  report.json       machine-readable, schema-versioned
  summary.md        GitHub step summary / PR comment body
  history.jsonl     one line appended per run (for trend charts)

The pass/fail logic imports eval.py unchanged, so numbers stay comparable
with manual runs of `python3 eval.py --url ...`.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
import time
from pathlib import Path

SCHEMA_VERSION = 1


def git_meta() -> dict:
    def g(*args: str) -> str:
        out = subprocess.run(["git", *args], capture_output=True, text=True)
        return out.stdout.strip()

    return {
        "sha": g("rev-parse", "HEAD"),
        "branch": g("rev-parse", "--abbrev-ref", "HEAD"),
        "message": g("log", "-1", "--pretty=%s"),
        "actor": os.environ.get("GITHUB_ACTOR", ""),
        "run_id": os.environ.get("GITHUB_RUN_ID", ""),
        "run_url": os.environ.get("GITHUB_SERVER_URL", "") + "/" + os.environ.get("GITHUB_REPOSITORY", "") + "/actions/runs/" + os.environ.get("GITHUB_RUN_ID", ""),
    }


def parse_eval_output(text: str) -> tuple[list[dict], int, int]:
    scenarios = []
    passed = 0
    for line in text.splitlines():
        if line.startswith("[PASS] ") or line.startswith("[FAIL] "):
            ok = line.startswith("[PASS]")
            name = line.removeprefix("[PASS] ").removeprefix("[FAIL] ").strip()
            if ok:
                passed += 1
            scenarios.append({"name": name, "ok": ok})
    total = len(scenarios)
    return scenarios, passed, total


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:18081/")
    parser.add_argument("--eval-cmd", default="python3 eval.py --url {url}")
    parser.add_argument("--output-dir", default="eval-artifacts")
    parser.add_argument("--label", default="local", help="report series label, e.g. pr-main or nightly")
    parser.add_argument("--fail-below", type=float, default=1.0,
                        help="exit 1 when pass rate is below this fraction")
    args = parser.parse_args()

    repo = Path.cwd()
    outdir = repo / args.output_dir
    outdir.mkdir(parents=True, exist_ok=True)

    cmd = args.eval_cmd.format(url=args.url)
    started = time.monotonic()
    proc = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=3600)
    wall_s = round(time.monotonic() - started, 1)

    scenarios, passed, total = parse_eval_output(proc.stdout)
    if total == 0:
        print(proc.stdout[-2000:], file=sys.stderr)
        print(proc.stderr[-2000:], file=sys.stderr)
        raise SystemExit("eval produced no parsable scenarios; see output above")

    pass_rate = round(passed / total, 4)
    report = {
        "schema_version": SCHEMA_VERSION,
        "label": args.label,
        "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "wall_s": wall_s,
        "exit_code": proc.returncode,
        "passed": passed,
        "total": total,
        "pass_rate": pass_rate,
        "scenarios": scenarios,
        "git": git_meta(),
    }

    report_path = outdir / "report.json"
    report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2))

    history_path = outdir / "history.jsonl"
    with history_path.open("a") as handle:
        handle.write(json.dumps(report, ensure_ascii=False) + "\n")

    lines = ["## Agent eval report", ""]
    emoji = "✅" if pass_rate >= args.fail_below else "❌"
    lines.append(f"{emoji} **{passed}/{total} passed** ({pass_rate:.0%}) — {args.label}, wall {wall_s}s")
    lines.append("")
    lines.append("| scenario | result |")
    lines.append("|---|---|")
    for sc in scenarios:
        lines.append(f"| {sc['name']} | {'✅' if sc['ok'] else '❌'} |")
    lines.append("")
    if args.label != "local":
        lines.append(f"run: {report['git']['run_url']}")
    (outdir / "summary.md").write_text("\n".join(lines))

    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_path:
        with open(summary_path, "a") as summary:
            summary.write("\n".join(lines))

    print(f"eval: {passed}/{total} passed ({pass_rate:.0%}), report at {report_path}")
    return 0 if pass_rate >= args.fail_below else 1


if __name__ == "__main__":
    sys.exit(main())
