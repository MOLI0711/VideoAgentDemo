#!/usr/bin/env python3
"""Score annotated saved runs. Never infer semantic correctness from a citation count."""
import argparse
import json
import math
from pathlib import Path


def overlap(a, b):
    start = max(a[0], b[0])
    end = min(a[1], b[1])
    union = max(a[1], b[1]) - min(a[0], b[0])
    return max(0, end - start) / union if union > 0 else 0


def score(expected, actual):
    ids = [row["id"] for row in expected]
    if len(set(ids)) != len(ids):
        raise ValueError("duplicate annotation IDs")
    indexed = {row["id"]: row for row in actual}
    if len(indexed) != len(actual):
        raise ValueError("duplicate result IDs")
    judged, correct, recalls, latency = 0, 0, [], []
    missing, mock, unknown_usage, failures = [], 0, 0, 0
    for gold in expected:
        row = indexed.get(gold["id"])
        if row is None:
            missing.append(gold["id"])
            continue
        answer = row.get("result") or {}
        mock += bool(answer.get("mock") or row.get("mock"))
        failures += row.get("status") != "SUCCEEDED"
        if row.get("usage_known") is not True:
            unknown_usage += 1
        if row.get("latency_ms") is not None:
            latency.append(float(row["latency_ms"]))
        # A reviewer supplies this after inspecting claim text and actual video.
        if isinstance(row.get("human_grounded"), bool):
            judged += 1
            correct += row["human_grounded"]
        intervals = gold.get("expected_intervals", [])
        evidence = answer.get("evidence", [])
        predicted = [[e["start_ms"], e["end_ms"]] for e in evidence
                     if isinstance(e.get("start_ms"), (float, int))
                     and isinstance(e.get("end_ms"), (float, int))]
        if intervals:
            hits = sum(any(overlap(g, p) >= 0.3 for p in predicted) for g in intervals)
            recalls.append(hits / len(intervals))
    latency.sort()
    return {
        "annotated_cases": len(expected), "observed_cases": len(actual),
        "missing_ids": missing, "failed_runs": failures, "mock_runs": mock,
        "runs_with_unknown_usage": unknown_usage,
        "citation_interval_recall_iou_0_3": sum(recalls) / len(recalls) if recalls else None,
        "human_grounded_rate": correct / judged if judged else None,
        "human_judged_cases": judged,
        "latency_p50_ms": latency[math.ceil(len(latency) * .5) - 1] if latency else None,
        "latency_p95_ms": latency[math.ceil(len(latency) * .95) - 1] if latency else None,
        "note": "Citation overlap is not semantic grounding. Missing annotations/usage remain unknown.",
    }


def rows(path):
    return [json.loads(line) for line in Path(path).read_text().splitlines() if line.strip()]


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("expected")
    parser.add_argument("actual")
    args = parser.parse_args()
    print(json.dumps(score(rows(args.expected), rows(args.actual)), ensure_ascii=False, indent=2))
