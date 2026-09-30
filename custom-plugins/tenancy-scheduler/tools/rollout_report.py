"""Compare cumulative scheduler snapshots without claiming counterfactual utility."""

import argparse
import datetime
import json
import math
import sys


COUNTERS = (
    "decisions", "agreements", "inadmissible_decisions",
    "unknown_candidate_decisions", "stale_window_decisions",
    "unpriced_decisions", "stale_price_decisions", "usage_records",
    "usage_successes", "usage_failures", "usage_429", "usage_with_quota",
    "usage_without_quota", "reservations_created", "reservations_released",
    "reservations_expired", "reservations_invalidated", "reservations_pruned",
    "unmatched_usage", "boundary_usage", "prior_epoch_reservations_removed",
)


def timestamp(value):
    if not isinstance(value, str):
        raise ValueError("timestamp must be a string")
    result = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        raise ValueError("timestamps must include timezone")
    return result


def subtract(a, b, name, integer=True):
    if not isinstance(a, (int, float)) or isinstance(a, bool) or not math.isfinite(a):
        raise ValueError(f"invalid counter {name}")
    if not isinstance(b, (int, float)) or isinstance(b, bool) or not math.isfinite(b):
        raise ValueError(f"invalid counter {name}")
    if a < 0 or b < a:
        raise ValueError(f"counter rollback: {name}")
    if integer and (not isinstance(a, int) or not isinstance(b, int)):
        raise ValueError(f"counter must be integer: {name}")
    return b - a


def interval(start, end):
    if not isinstance(start, dict) or not isinstance(end, dict):
        raise ValueError("snapshot must be an object")
    a, b = start["telemetry"], end["telemetry"]
    if not isinstance(a, dict) or not isinstance(b, dict):
        raise ValueError("telemetry must be an object")
    for key in ("schema_version", "epoch_id", "mode", "started_at", "config_fingerprint"):
        if key not in a or key not in b or a[key] != b[key]:
            raise ValueError(f"incompatible interval: {key}")
    if a["schema_version"] != 1 or not a["epoch_id"] or a["config_fingerprint"] != b["config_fingerprint"]:
        raise ValueError("unsupported or missing telemetry epoch")
    first, last = timestamp(start["saved_at"]), timestamp(end["saved_at"])
    if last <= first or timestamp(a["started_at"]) > first:
        raise ValueError("invalid interval timestamps")
    delta = {name: subtract(a[name], b[name], name) for name in COUNTERS}
    if delta["usage_successes"] + delta["usage_failures"] != delta["usage_records"]:
        raise ValueError("inconsistent usage totals")
    if delta["usage_429"] > delta["usage_failures"]:
        raise ValueError("429 count exceeds failures")
    if delta["agreements"] > delta["decisions"]:
        raise ValueError("agreements exceed decisions")
    histograms = {}
    for name in ("latency", "ttft"):
        x, y = a[name], b[name]
        if not isinstance(x, dict) or not isinstance(y, dict):
            raise ValueError("histogram must be an object")
        bounds = x["upper_bounds_ms"]
        if not isinstance(bounds, list) or not isinstance(x["counts"], list) or not isinstance(y["counts"], list):
            raise ValueError("histogram bounds/counts must be arrays")
        if bounds != y["upper_bounds_ms"] or len(x["counts"]) != len(bounds) + 1 or len(y["counts"]) != len(x["counts"]):
            raise ValueError(f"incompatible {name} histogram")
        if any(not isinstance(v, (int, float)) or not math.isfinite(v) or v <= 0 for v in bounds) or bounds != sorted(set(bounds)):
            raise ValueError(f"invalid {name} bounds")
        counts = [subtract(p, q, name) for p, q in zip(x["counts"], y["counts"])]
        samples = subtract(x["samples"], y["samples"], name)
        total = subtract(x["sum_ms"], y["sum_ms"], name, integer=False)
        if samples != sum(counts) or samples > delta["usage_records"]:
            raise ValueError(f"inconsistent {name} samples")
        p95, cumulative = None, 0
        for index, count in enumerate(counts):
            cumulative += count
            if samples and cumulative >= math.ceil(samples * .95):
                p95 = bounds[index] if index < len(bounds) else "overflow"
                break
        histograms[name] = {"samples": samples, "mean_ms": total / samples if samples else None, "p95_upper_bound_ms": p95}
    return {"mode": a["mode"], "epoch_id": a["epoch_id"], "config_fingerprint": a["config_fingerprint"],
            "start": start["saved_at"], "end": end["saved_at"], "seconds": (last-first).total_seconds(),
            "counts": delta, **histograms}


def wilson(k, n):
    if not n:
        return None
    z = 1.96
    p, denominator = k/n, 1 + z*z/n
    center = (p + z*z/(2*n))/denominator
    radius = z*math.sqrt(p*(1-p)/n + z*z/(4*n*n))/denominator
    return [max(0, center-radius), min(1, center+radius)]


def rates(data):
    c = data["counts"]
    result = {}
    for label, numerator, denominator in (
        ("failure", "usage_failures", "usage_records"),
        ("http_429", "usage_429", "usage_records"),
        ("inadmissible", "inadmissible_decisions", "decisions"),
        ("unknown_candidate", "unknown_candidate_decisions", "decisions"),
        ("stale_window", "stale_window_decisions", "decisions"),
        ("unpriced", "unpriced_decisions", "decisions"),
        ("stale_price", "stale_price_decisions", "decisions"),
        ("unmatched_usage", "unmatched_usage", "usage_records"),
    ):
        n, k = c[denominator], c[numerator]
        # Expiry may release work created before the interval; never invent a binomial rate.
        if k > n:
            result[label] = {"rate": None, "ci95": None, "note": "cross-interval lifecycle counts"}
        else:
            result[label] = {"rate": k/n if n else None, "ci95": wilson(k, n)}
    return result


def compare(baseline, canary, gates, min_samples):
    if not isinstance(gates, dict):
        raise ValueError("gates must be an object")
    reasons = []
    if baseline["mode"] != "shadow" or canary["mode"] != "optimizer":
        reasons.append("baseline must be shadow and canary optimizer")
    if baseline["config_fingerprint"] != canary["config_fingerprint"]:
        reasons.append("optimizer configuration differs between baseline and canary")
    for data in (baseline, canary):
        if data["counts"]["boundary_usage"]:
            reasons.append(f"{data['mode']}: usage crossed a measurement epoch boundary")
        if data["counts"]["prior_epoch_reservations_removed"]:
            reasons.append(f"{data['mode']}: prior-epoch reservation activity in interval")
        if data["counts"]["usage_records"] < min_samples or data["counts"]["decisions"] < min_samples:
            reasons.append(f"{data['mode']}: insufficient samples")
        if data["latency"]["samples"] < min_samples:
            reasons.append(f"{data['mode']}: insufficient latency samples")
    if not gates:
        reasons.append("operator regression gates are required")
    br, cr = rates(baseline), rates(canary)
    checks = {}
    for name, tolerance in gates.items():
        if name not in br or isinstance(tolerance, bool) or not isinstance(tolerance, (int, float)) or not math.isfinite(tolerance) or not 0 <= tolerance <= 1:
            raise ValueError(f"invalid rate gate: {name}")
        x, y = br[name], cr[name]
        if x["ci95"] is None or y["ci95"] is None:
            reasons.append(f"{name}: missing rate evidence")
            continue
        delta = y["rate"] - x["rate"]
        low, high = y["ci95"][0] - x["ci95"][1], y["ci95"][1] - x["ci95"][0]
        checks[name] = {"baseline_rate": x["rate"], "canary_rate": y["rate"], "absolute_delta": delta,
                        "conservative_delta_interval": [low, high], "max_absolute_regression": tolerance,
                        "verdict": "regression" if low > tolerance else "within_gate" if high <= tolerance else "uncertain"}
    required = {"failure", "http_429"}
    if not required.issubset(gates):
        reasons.append("failure and http_429 gates are required")
    status = "rollback" if any(c["verdict"] == "regression" for c in checks.values()) else "insufficient_evidence" if reasons else "continue_measuring" if any(c["verdict"] == "uncertain" for c in checks.values()) else "rate_gates_passed"
    return {"status": status, "reasons": reasons, "checks": checks,
            "baseline": {**baseline, "rates": br}, "canary": {**canary, "rates": cr},
            "limitations": ["Usage records are not logical requests; FIFO diagnostics are not exact attempt reconciliation.",
                            "Rate intervals assume independent comparable observations; traffic correlation and reset phase can invalidate this assumption.",
                            "Rate gates do not establish utility improvement, fairness, or latency safety; inspect histogram summaries and matched workload evidence before promotion."]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("baseline-start", "baseline-end", "canary-start", "canary-end"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--min-samples", type=int, required=True)
    parser.add_argument("--gates", required=True, help="JSON file: rate name -> maximum absolute increase (fraction)")
    args = parser.parse_args()
    try:
        if args.min_samples <= 0:
            raise ValueError("min-samples must be positive")
        def read(path):
            with open(path, encoding="utf-8") as source:
                return json.load(source)
        baseline = interval(read(args.baseline_start), read(args.baseline_end))
        canary = interval(read(args.canary_start), read(args.canary_end))
        report = compare(baseline, canary, read(args.gates), args.min_samples)
    except (ValueError, KeyError, TypeError, OSError, OverflowError) as error:
        report = {"status": "insufficient_evidence", "reasons": [str(error)]}
    print(json.dumps(report, indent=2, allow_nan=False))
    return 0 if report["status"] == "rate_gates_passed" else 2


if __name__ == "__main__":
    sys.exit(main())
