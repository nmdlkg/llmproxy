import copy
import unittest

import rollout_report as report


def snapshot(mode="shadow", epoch="one", count=0, end=False):
    t = {key: 0 for key in report.COUNTERS}
    t.update(schema_version=1, mode=mode, epoch_id=epoch,
             started_at="2026-09-30T00:00:00Z", config_fingerprint="config",
             decisions=count, usage_records=count, usage_successes=count,
             reservations_created=count, reservations_released=count)
    for name in ("latency", "ttft"):
        t[name] = {"upper_bounds_ms": [100, 1000], "counts": [count, 0, 0], "samples": count, "sum_ms": count*50}
    return {"saved_at": "2026-09-30T02:00:00Z" if end else "2026-09-30T01:00:00Z", "telemetry": t}


class ComparisonTests(unittest.TestCase):
    def test_interval_excludes_old_totals(self):
        data = report.interval(snapshot(count=100), snapshot(count=110, end=True))
        self.assertEqual(data["counts"]["usage_records"], 10)
        self.assertEqual(data["latency"]["p95_upper_bound_ms"], 100)

    def test_reload_and_mode_transition_rejected(self):
        for end in (snapshot(epoch="two", end=True), snapshot(mode="optimizer", end=True)):
            with self.assertRaises(ValueError):
                report.interval(snapshot(), end)

    def test_rollback_missing_and_invalid_histogram(self):
        with self.assertRaises(ValueError):
            report.interval(snapshot(count=100), snapshot(count=1, end=True))
        old = snapshot()
        del old["telemetry"]
        with self.assertRaises(KeyError):
            report.interval(old, snapshot(end=True))
        invalid = snapshot(count=20, end=True)
        invalid["telemetry"]["latency"]["counts"] = [0, 0, 0]
        with self.assertRaises(ValueError):
            report.interval(snapshot(), invalid)

    def test_insufficient_samples_never_pass(self):
        b = report.interval(snapshot(), snapshot(count=10, end=True))
        c = report.interval(snapshot(mode="optimizer", epoch="two"), snapshot(mode="optimizer", epoch="two", count=10, end=True))
        self.assertEqual(report.compare(b, c, {"failure": .01}, 100)["status"], "insufficient_evidence")

    def test_actual_429_regression(self):
        b = report.interval(snapshot(), snapshot(count=10000, end=True))
        last = snapshot(mode="optimizer", epoch="two", count=10000, end=True)
        last["telemetry"].update(usage_successes=9000, usage_failures=1000, usage_429=1000)
        c = report.interval(snapshot(mode="optimizer", epoch="two"), last)
        result = report.compare(b, c, {"http_429": .01}, 100)
        self.assertEqual(result["status"], "rollback")
        self.assertAlmostEqual(result["checks"]["http_429"]["absolute_delta"], .1)

    def test_expiry_is_diagnostic_only(self):
        b = report.interval(snapshot(), snapshot(count=100, end=True))
        b["counts"]["reservations_expired"] = 101
        self.assertNotIn("reservation_expiry", report.rates(b))

    def test_gates_missing_and_nan_rejected(self):
        b = report.interval(snapshot(), snapshot(count=10000, end=True))
        c = copy.deepcopy(b)
        c["mode"] = "optimizer"
        self.assertEqual(report.compare(b, c, {}, 100)["status"], "insufficient_evidence")
        with self.assertRaises(ValueError):
            report.compare(b, c, {"failure": float("nan")}, 100)
        with self.assertRaises(ValueError):
            report.compare(b, c, {"failure": True}, 100)

    def test_partial_gates_and_config_changes_cannot_pass(self):
        b = report.interval(snapshot(), snapshot(count=10000, end=True))
        c = copy.deepcopy(b)
        c["mode"] = "optimizer"
        self.assertEqual(report.compare(b, c, {"unpriced": .1}, 100)["status"], "insufficient_evidence")
        c["config_fingerprint"] = "changed"
        self.assertEqual(report.compare(b, c, {"failure": .1, "http_429": .1}, 100)["status"], "insufficient_evidence")

    def test_malformed_shapes_and_float_counts(self):
        for value in (None, [], 3):
            with self.assertRaises(ValueError):
                report.interval(value, snapshot(end=True))
        invalid = snapshot(count=10, end=True)
        invalid["telemetry"]["decisions"] = 10.5
        with self.assertRaises(ValueError):
            report.interval(snapshot(), invalid)
        with self.assertRaises(ValueError):
            report.timestamp(123)
        with self.assertRaises(ValueError):
            report.compare({}, {}, [], 10)

    def test_missing_latency_does_not_hide_clear_regression(self):
        b = report.interval(snapshot(), snapshot(count=10000, end=True))
        c = copy.deepcopy(b)
        c["mode"] = "optimizer"
        c["counts"].update(usage_successes=1000, usage_failures=9000, usage_429=9000)
        c["latency"]["samples"] = 0
        self.assertEqual(report.compare(b, c, {"failure": .1, "http_429": .1}, 100)["status"], "rollback")


if __name__ == "__main__":
    unittest.main()
