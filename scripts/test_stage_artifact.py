"""Exercise extraction and failure handling without a Docker daemon."""
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("stage_artifact", Path(__file__).with_name("stage_artifact.py"))
stage = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stage)


class StageArtifactTest(unittest.TestCase):
    def setUp(self):
        self.env = dict(GITHUB_SHA="a" * 40, GITHUB_REPOSITORY="fin-assist/user", GITHUB_RUN_ID="123")
        self.calls = []

    def docker(self, *args):
        self.calls.append(args)
        if args[:2] == ("image", "inspect"):
            return "linux/amd64" if args[3] == "{{.Os}}/{{.Architecture}}" else "sha256:" + "b" * 64
        if args[0] == "create":
            return "container-id"
        if args[0] == "cp":
            Path(args[2]).write_bytes(b"ELF-test-binary")
        return ""

    def test_package_binds_exact_image_source_and_binary(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", self.docker):
            result = stage.package("user", "user:ci", Path(tmp) / "out", self.env)
            self.assertEqual(result["source_sha"], "a" * 40)
            self.assertEqual(result["platform"], "linux/amd64")
            self.assertEqual(result["run_id"], 123)
            self.assertEqual(len(result["binary_sha256"]), 64)
            self.assertIn(("create", "sha256:" + "b" * 64), self.calls)
            self.assertIn(("rm", "container-id"), self.calls)
            self.assertTrue((Path(tmp) / "out/manifest.json").is_file())

    def test_invalid_metadata_rejected_before_docker(self):
        for key, value in [("GITHUB_SHA", "main"), ("GITHUB_REPOSITORY", "evil/repo"), ("GITHUB_RUN_ID", "x")]:
            with self.subTest(key=key), tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", self.docker):
                with self.assertRaises(ValueError):
                    stage.package("user", "user:ci", Path(tmp) / "out", self.env | {key: value})
                self.assertEqual(self.calls, [])

    def test_service_cannot_escape_output(self):
        with self.assertRaises(ValueError):
            stage.package("../user", "user:ci", Path("unused"), self.env)

    def test_container_removed_when_copy_fails(self):
        def fail_copy(*args):
            if args[0] == "cp":
                raise RuntimeError("copy failed")
            return self.docker(*args)
        with tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", fail_copy):
            with self.assertRaises(RuntimeError):
                stage.package("user", "user:ci", Path(tmp) / "out", self.env)
            self.assertIn(("rm", "container-id"), self.calls)

    def test_foreign_platform_rejected(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", return_value="linux/arm64"):
            with self.assertRaises(ValueError):
                stage.package("user", "user:ci", Path(tmp) / "out", self.env)

    def test_existing_directory_not_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", self.docker):
            with self.assertRaises(FileExistsError):
                stage.package("user", "user:ci", Path(tmp), self.env)
