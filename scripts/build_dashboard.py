"""Build a self-contained HTML trend dashboard from eval history JSONL."""

from __future__ import annotations

import argparse
import json
from pathlib import Path


def sparkline(values: list[float], width: int = 260, height: int = 40) -> str:
    if not values:
        return ""
    lo, hi = min(values), max(values)
    span = (hi - lo) or 1.0
    step = width / max(len(values) - 1, 1)
    points = []
    for i, v in enumerate(values):
        x = i * step
        y = height - ((v - lo) / span) * (height - 6) - 3
        points.append(f"{x:.1f},{y:.1f}")
    return (
        f'<svg width="{width}" height="{height}" class="spark">'
        f'<polyline points="{" ".join(points)}" fill="none" stroke="currentColor" stroke-width="2"/>'
        f"</svg>"
    )


def render(runs: list[dict]) -> str:
    runs = sorted(runs, key=lambda r: r["ts"])
    rates = [r["pass_rate"] for r in runs]
    rows = []
    for r in runs[-30:]:
        lf = r.get("langfuse") or {}
        run_cell = f'<a href="{lf["dataset_run_url"]}">langfuse</a>' if lf.get("dataset_run_url") else ""
        rows.append(
            "<tr>"
            f"<td>{r['ts']}</td>"
            f"<td>{r['label']}</td>"
            f"<td>{r['passed']}/{r['total']}</td>"
            f"<td>{r['pass_rate']:.0%}</td>"
            f"<td>{r['wall_s']}s</td>"
            f"<td><code>{(r.get('git') or {}).get('sha', '')[:7]}</code></td>"
            f"<td>{run_cell}</td>"
            "</tr>"
        )
    scenario_stats = {}
    for r in runs:
        for sc in r.get("scenarios", []):
            stats = scenario_stats.setdefault(sc["name"], {"pass": 0, "total": 0})
            stats["total"] += 1
            stats["pass"] += 1 if sc["ok"] else 0
    scenario_rows = []
    for name, stats in scenario_stats.items():
        rate = stats["pass"] / stats["total"]
        bar = "█" * int(rate * 20) + "░" * (20 - int(rate * 20))
        scenario_rows.append(f"<tr><td>{name}</td><td>{bar} {rate:.0%}</td></tr>")

    return f"""<!doctype html>
<html><head><meta charset="utf-8"><title>agentrun eval trends</title>
<style>
 body {{ font-family: -apple-system, Segoe UI, sans-serif; margin: 2rem; color: #1a1a2e; }}
 h1 {{ font-size: 1.4rem; }}
 .big {{ font-size: 3rem; font-weight: 700; }}
 .spark {{ color: #2563eb; }}
 table {{ border-collapse: collapse; margin-top: 1.5rem; width: 100%; max-width: 860px; }}
 td, th {{ border-bottom: 1px solid #e5e7eb; padding: 6px 10px; text-align: left; font-size: 0.9rem; }}
 code {{ background: #f3f4f6; padding: 1px 4px; }}
</style></head><body>
<h1>agentrun eval trends</h1>
<div class="big">{rates[-1]:.0%}</div>
<div>{runs[-1]['passed']}/{runs[-1]['total']} passed, last run {runs[-1]['ts']} ({runs[-1]['label']})</div>
<div>{sparkline(rates)}</div>
<h2>Scenario stability (all history)</h2>
<table><tr><th>scenario</th><th>pass rate</th></tr>{''.join(scenario_rows)}</table>
<h2>Recent runs</h2>
<table><tr><th>ts</th><th>label</th><th>score</th><th>rate</th><th>wall</th><th>commit</th><th>trace</th></tr>{''.join(rows)}</table>
</body></html>"""


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--history", required=True)
    parser.add_argument("--out", required=True)
    args = parser.parse_args()

    runs = []
    path = Path(args.history)
    if path.exists():
        for line in path.read_text().splitlines():
            if line.strip():
                runs.append(json.loads(line))
    if not runs:
        raise SystemExit(f"no runs in {path}")

    Path(args.out).write_text(render(runs))
    print(f"dashboard: {args.out} ({len(runs)} runs)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
