"""The race-enabled collectors must reject tests outside the three suites."""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import tempfile
import unittest


MAKEFILE = Path(__file__).resolve().parents[1] / "Makefile"


class SuiteSelectionTests(unittest.TestCase):
    def run_make(self, root, target):
        process = subprocess.Popen(
            ["make", target], cwd=root, stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT, text=True, start_new_session=True,
        )
        try:
            output, _ = process.communicate(timeout=60)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate()
            self.fail("suite classification exceeded 60 seconds")
        return process.returncode, output

    def test_race_only_unclassified_test_blocks_collection(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copyfile(MAKEFILE, root / "Makefile")
            (root / "go.mod").write_text("module example.com/selection\n\ngo 1.26.0\n")
            (root / "rule_test.go").write_text(
                'package selection\nimport "testing"\n'
                'func TestUnitRule(t *testing.T) {}\n'
                'func TestIntegrationRule(t *testing.T) {}\n'
                'func TestAPIRule(t *testing.T) {}\n'
            )
            status, output = self.run_make(root, "test-names")
            self.assertEqual(status, 0, output)
            (root / "race_test.go").write_text(
                '//go:build race\n\npackage selection\nimport "testing"\n'
                'func TestRaceRegression(t *testing.T) { t.Fatal("must not be omitted") }\n'
            )
            for target in ["test-names", "collect-unit", "cover"]:
                with self.subTest(target=target):
                    status, output = self.run_make(root, target)
                    self.assertNotEqual(status, 0, output)
                    self.assertIn("TestRaceRegression", output)
                    self.assertIn("Name every test", output)
                    self.assertFalse(list(root.glob("*.jsonl")))
                    self.assertFalse(list(root.glob("*.out")))


if __name__ == "__main__":
    unittest.main()
