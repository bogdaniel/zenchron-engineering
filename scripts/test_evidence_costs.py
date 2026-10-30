import tempfile
import unittest
from pathlib import Path

from evidence_costs import observe, profile


def line(test, seconds, action="pass"):
    return '2026-10-30T11:40:00.0000000Z {"Time":"t","Action":"%s","Package":"p","Test":"%s","Elapsed":%s}\n' % (action, test, seconds)


class EvidenceCostsTest(unittest.TestCase):
    def sample(self, *entries):
        directory = Path(tempfile.mkdtemp())
        (directory / "shard.log").write_text("".join(entries) + "not json\n")
        return observe(directory)

    def test_subtests_and_noise_are_not_tests(self):
        passed, failed = self.sample(line("TestA", 2.0), line("TestA/sub", 9.0), line("TestB", 0.5, "fail"))
        self.assertEqual((passed, failed), ({"TestA": 2.0}, {"TestB"}))

    def test_cost_is_the_median_so_one_spike_does_not_decide_and_raw_values_are_kept(self):
        samples = [self.sample(line("TestA", s), line("TestB", 0.1)) for s in (10.0, 11.0, 700.0)]
        result, raw, report = profile(samples, 1.0)
        self.assertEqual(result["tests"], {"TestA": 11.0})
        self.assertEqual(result["default"], 0.1)
        self.assertEqual(raw, {"TestA": [10.0, 11.0, 700.0]})
        self.assertEqual(report["failed"], {})
        self.assertEqual(report["incomplete"], {})

    def test_a_failed_or_missing_observation_is_reported_and_is_not_a_cost(self):
        samples = [self.sample(line("TestA", 5.0)), self.sample(line("TestA", 900.0, "fail")), self.sample()]
        result, raw, report = profile(samples, 1.0)
        self.assertEqual(result["tests"], {"TestA": 5.0})
        self.assertEqual(raw["TestA"], [5.0, None, None])
        self.assertEqual(report["failed"], {"TestA": [1]})
        self.assertEqual(report["incomplete"], {"TestA": [1, 2]})


if __name__ == "__main__":
    unittest.main()
