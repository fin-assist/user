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


    def make_module(self, root):
        shutil.copyfile(MAKEFILE, root / "Makefile")
        (root / "go.mod").write_text("module example.com/selection\n\ngo 1.26.0\n")
        (root / "rule_test.go").write_text(
            'package selection\nimport "testing"\n'
            'func TestUnitRule(t *testing.T) {}\n'
        )

    def test_unclassified_fuzz_target_blocks_collection(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_module(root)
            (root / "fuzz_test.go").write_text(
                'package selection\nimport "testing"\n'
                'func FuzzParse(f *testing.F) { f.Add("x"); f.Fuzz(func(t *testing.T, s string) {}) }\n'
            )
            for target in ["test-names", "collect-unit"]:
                with self.subTest(target=target):
                    status, output = self.run_make(root, target)
                    self.assertNotEqual(status, 0, output)
                    self.assertIn("FuzzParse", output)
                    self.assertIn("every fuzz target", output)
                    self.assertFalse(list(root.glob("*.jsonl")))

    def test_classified_fuzz_seed_runs_in_its_suite_only(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_module(root)
            (root / "parse.go").write_text(
                'package selection\n\nfunc Parse(s string) int { return len(s) }\n'
            )
            (root / "fuzz_test.go").write_text(
                'package selection\nimport "testing"\n'
                'func FuzzUnitParse(f *testing.F) { f.Add("seed"); f.Fuzz(func(t *testing.T, s string) { Parse(s) }) }\n'
            )
            (root / "scripts").mkdir()
            shutil.copyfile(MAKEFILE.parent / "scripts" / "check-suite.sh", root / "scripts" / "check-suite.sh")
            status, output = self.run_make(root, "collect-unit")
            self.assertEqual(status, 0, output)
            events = (root / "unit.jsonl").read_text()
            self.assertIn('"Action":"pass","Package":"example.com/selection","Test":"FuzzUnitParse"', events.replace(" ", ""))
            status, output = self.run_make(root, "collect-integration")
            self.assertNotEqual(status, 0, output)
            self.assertIn("Suite must execute", output)
            integration = (root / "integration.jsonl").read_text()
            self.assertIn('"Package":"example.com/selection"', integration.replace(" ", ""))
            self.assertNotIn("FuzzUnitParse", integration)

    def test_example_blocks_collection(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.make_module(root)
            (root / "example_test.go").write_text(
                'package selection\nimport "fmt"\n'
                'func Example() { fmt.Println("x")\n\t// Output: x\n}\n'
            )
            status, output = self.run_make(root, "test-names")
            self.assertNotEqual(status, 0, output)
            self.assertIn("Examples run outside the three suites", output)


if __name__ == "__main__":
    unittest.main()
