"""Run the Langfuse dataset as an experiment against a live agentrun agent.

Single source of truth: the cases live in a Langfuse dataset, every turn is a
traced A2A call, every assertion becomes a Langfuse score on the item's trace,
and the same numbers are written to ``report.json`` / ``summary.md`` /
``history.jsonl`` for the CI gate and the static dashboard. There is no second
scoring path: the CI gate reads what Langfuse stores.

Usage:
    python3 scripts/langfuse_experiment.py \
        --dataset tg-exchange-support \
        --url http://127.0.0.1:18081/ \
        --run-name "ci-$GITHUB_RUN_ID" \
        --output-dir eval-artifacts \
        --fail-below 0.1
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import time
import urllib.request
import uuid
from pathlib import Path

from langfuse import Langfuse, observe

SCHEMA_VERSION = 2

AGENT_URL = ""
REQUEST_TIMEOUT = 120.0


def normalize(text: str) -> str:
    return text.replace("\u2011", "-").replace("\u202f", " ").replace("\u00a0", " ").lower()


@observe(as_type="generation", name="a2a-turn")
def a2a_turn(text: str, tag: str, context_id: str | None) -> tuple[str, str | None]:
    meta = f"[telegram_username: {tag} | first_name: Eval | chat_id: eval_{tag}]\n"
    message = {"role": "user", "messageId": uuid.uuid4().hex, "parts": [{"kind": "text", "text": meta + text}]}
    if context_id:
        message["contextId"] = context_id
    body = {"jsonrpc": "2.0", "id": uuid.uuid4().hex, "method": "message/send", "params": {"message": message}}
    req = urllib.request.Request(AGENT_URL, data=json.dumps(body).encode(), method="POST",
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=REQUEST_TIMEOUT) as resp:
        data = json.loads(resp.read().decode())
    if data.get("error"):
        raise RuntimeError(f"a2a error: {data['error']}")
    result = data.get("result") or {}
    reply = "\n".join(p.get("text", "") for a in result.get("artifacts", []) for p in a.get("parts", []))
    return normalize(reply), result.get("contextId") or context_id


def task(*, item, **kwargs) -> dict:
    """Replay one scenario's turns and return the last reply plus the transcript."""
    turns = item.input["turns"]
    tag = item.input.get("tag") or "eval"
    unique_tag = f"{tag}_{uuid.uuid4().hex[:6]}"
    context_id = None
    transcript = []
    started = time.monotonic()
    for turn in turns:
        reply, context_id = a2a_turn(turn, unique_tag, context_id)
        transcript.append({"user": turn, "agent": reply})
    return {"reply": transcript[-1]["agent"] if transcript else "",
            "transcript": transcript,
            "latency_s": round(time.monotonic() - started, 2)}


def contains_any(text: str, markers: list[str]) -> bool:
    return any(m in text for m in markers)


def scenario_evaluator(*, input, output, expected_output, metadata, **kwargs) -> list:
    """Marker assertions from the dataset item, scored as Langfuse evaluations."""
    from langfuse import Evaluation

    reply = (output or {}).get("reply", "")
    expect = expected_output or {}
    evaluations = []

    checks_passed = True
    if expect.get("any_of"):
        ok = contains_any(reply, expect["any_of"])
        checks_passed = checks_passed and ok
        evaluations.append(Evaluation(name="expect_any_of", value=ok, data_type="BOOLEAN",
                                      comment=None if ok else f"none of {expect['any_of'][:4]}... matched"))
    if expect.get("also_any_of"):
        ok = contains_any(reply, expect["also_any_of"])
        checks_passed = checks_passed and ok
        evaluations.append(Evaluation(name="expect_also_any_of", value=ok, data_type="BOOLEAN",
                                      comment=None if ok else f"none of {expect['also_any_of'][:4]}... matched"))
    if expect.get("none_of"):
        hit = [m for m in expect["none_of"] if m in reply]
        ok = not hit
        checks_passed = checks_passed and ok
        evaluations.append(Evaluation(name="expect_none_of", value=ok, data_type="BOOLEAN",
                                      comment=None if ok else f"forbidden markers present: {hit}"))

    leak_markers = expect.get("leak_markers") or []
    leaked = [m for m in leak_markers if m in reply]
    if leak_markers:
        evaluations.append(Evaluation(name="no_internals_leak", value=not leaked, data_type="BOOLEAN",
                                      comment=None if not leaked else f"internals leaked: {leaked}"))
    checks_passed = checks_passed and not leaked

    evaluations.append(Evaluation(name="scenario_pass", value=checks_passed, data_type="BOOLEAN"))
    evaluations.append(Evaluation(name="latency_s", value=(output or {}).get("latency_s", 0.0),
                                  data_type="NUMERIC"))
    return evaluations


def run_evaluator(*, item_results, **kwargs) -> list:
    """Aggregate pass rate across the whole dataset run."""
    from langfuse import Evaluation

    total = len(item_results)
    passed = 0
    for r in item_results:
        for ev in r.evaluations:
            if ev.name == "scenario_pass" and ev.value:
                passed += 1
    rate = passed / total if total else 0.0
    return [
        Evaluation(name="pass_rate", value=rate, data_type="NUMERIC",
                   comment=f"{passed}/{total} scenarios passed"),
        Evaluation(name="scenarios_total", value=float(total), data_type="NUMERIC"),
    ]


def git_meta() -> dict:
    def g(*args: str) -> str:
        out = subprocess.run(["git", *args], capture_output=True, text=True)
        return out.stdout.strip()

    server = os.environ.get("GITHUB_SERVER_URL", "")
    repo = os.environ.get("GITHUB_REPOSITORY", "")
    run_id = os.environ.get("GITHUB_RUN_ID", "")
    return {
        "sha": g("rev-parse", "HEAD"),
        "branch": g("rev-parse", "--abbrev-ref", "HEAD"),
        "run_url": f"{server}/{repo}/actions/runs/{run_id}" if run_id else "",
    }


def main() -> int:
    global AGENT_URL

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dataset", required=True)
    parser.add_argument("--url", default="http://127.0.0.1:18081/")
    parser.add_argument("--run-name", default=None)
    parser.add_argument("--label", default="local")
    parser.add_argument("--output-dir", default="eval-artifacts")
    parser.add_argument("--fail-below", type=float, default=0.0)
    parser.add_argument("--max-concurrency", type=int, default=2)
    args = parser.parse_args()

    AGENT_URL = args.url

    client = Langfuse(
        public_key=os.environ["LANGFUSE_PUBLIC_KEY"],
        secret_key=os.environ["LANGFUSE_SECRET_KEY"],
        host=os.environ.get("LANGFUSE_HOST", "https://cloud.langfuse.com"),
    )
    dataset = client.get_dataset(args.dataset)

    started = time.monotonic()
    result = dataset.run_experiment(
        name=f"agentrun {args.label}",
        run_name=args.run_name,
        description=f"agentrun CI eval, label={args.label}",
        task=task,
        evaluators=[scenario_evaluator],
        run_evaluators=[run_evaluator],
        max_concurrency=args.max_concurrency,
        metadata={"label": args.label, **git_meta()},
    )
    wall_s = round(time.monotonic() - started, 1)
    client.flush()

    scenarios = []
    passed = 0
    for r in result.item_results:
        name = (r.item.metadata or {}).get("scenario") or r.item.id
        ok = any(ev.name == "scenario_pass" and ev.value for ev in r.evaluations)
        failures = [ev.comment for ev in r.evaluations if ev.data_type == "BOOLEAN" and not ev.value and ev.comment]
        latency = next((ev.value for ev in r.evaluations if ev.name == "latency_s"), None)
        scenarios.append({"name": name, "ok": bool(ok), "latency_s": latency,
                          "failures": failures,
                          "trace_id": getattr(r, "trace_id", None)})
        passed += 1 if ok else 0

    total = len(scenarios)
    pass_rate = round(passed / total, 4) if total else 0.0

    report = {
        "schema_version": SCHEMA_VERSION,
        "label": args.label,
        "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "wall_s": wall_s,
        "passed": passed,
        "total": total,
        "pass_rate": pass_rate,
        "scenarios": scenarios,
        "langfuse": {
            "dataset": args.dataset,
            "run_name": result.run_name,
            "dataset_run_id": result.dataset_run_id,
            "dataset_run_url": result.dataset_run_url,
        },
        "git": git_meta(),
    }

    outdir = Path(args.output_dir)
    outdir.mkdir(parents=True, exist_ok=True)
    (outdir / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2))
    with (outdir / "history.jsonl").open("a") as handle:
        handle.write(json.dumps(report, ensure_ascii=False) + "\n")

    emoji = "✅" if pass_rate >= args.fail_below else "❌"
    lines = [
        "## Agent eval report",
        "",
        f"{emoji} **{passed}/{total} passed** ({pass_rate:.0%}) — {args.label}, wall {wall_s}s",
        "",
        "| scenario | result | latency | detail |",
        "|---|---|---|---|",
    ]
    for sc in scenarios:
        detail = "; ".join(f for f in sc["failures"] if f)[:120] or ""
        lat = f"{sc['latency_s']:.1f}s" if sc["latency_s"] else ""
        lines.append(f"| {sc['name']} | {'✅' if sc['ok'] else '❌'} | {lat} | {detail} |")
    lines += ["", f"Langfuse run: {report['langfuse']['dataset_run_url'] or report['langfuse']['run_name']}"]
    if report["git"]["run_url"]:
        lines.append(f"CI run: {report['git']['run_url']}")

    summary = "\n".join(lines)
    (outdir / "summary.md").write_text(summary)
    step_summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if step_summary:
        with open(step_summary, "a") as handle:
            handle.write(summary + "\n")

    print(f"eval: {passed}/{total} passed ({pass_rate:.0%})")
    print(f"langfuse: {report['langfuse']['dataset_run_url']}")
    return 0 if pass_rate >= args.fail_below else 1


if __name__ == "__main__":
    raise SystemExit(main())
