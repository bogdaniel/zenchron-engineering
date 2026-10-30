"""Per-test cost profile for ci/evidence/costs.json from hosted `go test -json` logs.

usage: evidence_costs.py MIN_SECONDS SAMPLE_DIR [SAMPLE_DIR ...]

Each SAMPLE_DIR holds the job logs of one hosted run (all shards of that run).
A test's cost is the median of its top-level durations across the samples that
passed it. Tests at or above MIN_SECONDS are listed; `default` is the mean of
the rest, which is what the evidence tool charges any test the profile does not
name. The output keeps what a profile alone would hide: every listed test's raw
observation per sample, and a report of tests that failed in a sample (their
duration is not a cost and is excluded) or are missing from some samples.
"""
import json
import statistics
import sys
from pathlib import Path


def observe(sample):
    """The passed and failed top-level tests of one sample, by name."""
    passed, failed = {}, set()
    for log in sorted(Path(sample).glob("*.log")):
        for line in log.read_text(errors="replace").splitlines():
            start = line.find('{"Time"')
            if start < 0:
                continue
            try:
                event = json.loads(line[start:])
            except ValueError:
                continue
            test, action = event.get("Test"), event.get("Action")
            if not test or "/" in test or action not in ("pass", "fail"):
                continue
            if action == "fail":
                failed.add(test)
            else:
                passed[test] = event.get("Elapsed", 0.0)
    return passed, failed


def profile(samples, minimum):
    observed = [passed for passed, _ in samples]
    names = set().union(*observed) | set().union(*(failed for _, failed in samples))
    raw = {test: [sample.get(test) for sample in observed] for test in sorted(names)}
    cost = {test: statistics.median(v for v in values if v is not None)
            for test, values in raw.items() if any(v is not None for v in values)}
    listed = {test: round(seconds, 1) for test, seconds in sorted(cost.items()) if round(seconds, 1) >= minimum}
    rest = [seconds for test, seconds in cost.items() if test not in listed]
    default = round(statistics.mean(rest), 3) if rest else minimum
    report = {
        "samples": len(samples),
        "failed": {test: [i for i, (_, failed) in enumerate(samples) if test in failed]
                   for test in sorted(set().union(*(failed for _, failed in samples)))},
        "incomplete": {test: [i for i, v in enumerate(values) if v is None]
                       for test, values in raw.items() if any(v is None for v in values)},
    }
    return {"default": max(default, 0.001), "tests": listed}, {t: raw[t] for t in listed}, report


if __name__ == "__main__":
    minimum = float(sys.argv[1])
    profile_, observations, report = profile([observe(d) for d in sys.argv[2:]], minimum)
    print(json.dumps({"profile": profile_, "observations": observations, "report": report}, indent=1))
