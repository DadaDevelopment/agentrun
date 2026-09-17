"""Sync a repo-owned eval case file into a Langfuse dataset.

The YAML in ``evals/`` is the source of truth for the cases; Langfuse is the
source of truth for the results. Items are addressed by a deterministic id
derived from the scenario id, so the sync is idempotent: re-running updates
existing items in place instead of duplicating them.

Usage:
    python3 scripts/langfuse_sync_dataset.py \
        --file evals/tg-exchange-support.yaml \
        --dataset tg-exchange-support
"""

from __future__ import annotations

import argparse
import os
import sys

import yaml
from langfuse import Langfuse


def load_cases(path: str) -> tuple[list[dict], list[str]]:
    data = yaml.safe_load(open(path, encoding="utf-8"))
    scenarios = data.get("scenarios") or []
    if not scenarios:
        raise SystemExit(f"{path}: no scenarios")
    return scenarios, data.get("leak_markers") or []


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--file", required=True)
    parser.add_argument("--dataset", required=True)
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    scenarios, leak_markers = load_cases(args.file)

    client = Langfuse(
        public_key=os.environ["LANGFUSE_PUBLIC_KEY"],
        secret_key=os.environ["LANGFUSE_SECRET_KEY"],
        host=os.environ.get("LANGFUSE_HOST", "https://cloud.langfuse.com"),
    )

    if args.dry_run:
        for sc in scenarios:
            print(f"would sync {args.dataset}/{sc['id']} ({len(sc['turns'])} turns)")
        return 0

    client.create_dataset(
        name=args.dataset,
        description=f"agentrun eval cases, synced from {args.file}",
        metadata={"source_file": args.file, "leak_markers": leak_markers},
    )

    for sc in scenarios:
        client.create_dataset_item(
            dataset_name=args.dataset,
            id=f"{args.dataset}::{sc['id']}",
            input={"turns": sc["turns"], "tag": sc["id"]},
            expected_output={**(sc.get("expect") or {}), "leak_markers": leak_markers},
            metadata={"scenario": sc["id"], "tags": sc.get("tags") or []},
        )
        print(f"synced {sc['id']}")

    client.flush()
    print(f"dataset {args.dataset}: {len(scenarios)} items")
    return 0


if __name__ == "__main__":
    sys.exit(main())
