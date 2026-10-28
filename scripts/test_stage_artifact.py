"""Exercise extraction and failure handling without a Docker daemon."""
import importlib.util
import hashlib
import json
import os
import re
import subprocess
import sys
import yaml
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("stage_artifact", Path(__file__).with_name("stage_artifact.py"))
stage = importlib.util.module_from_spec(spec)
spec.loader.exec_module(stage)


BINARY = b"ELF-test-binary" + bytes(range(256)) * 9000


class StageArtifactTest(unittest.TestCase):
    def setUp(self):
        self.env = dict(GITHUB_SHA="a" * 40, GITHUB_REPOSITORY="fin-assist/user", GITHUB_RUN_ID="123", GITHUB_RUN_ATTEMPT="2",
                        GITHUB_EVENT_NAME="push", GITHUB_REF="refs/heads/main")
        self.calls = []

    def docker(self, *args):
        self.calls.append(args)
        if args[:2] == ("image", "inspect"):
            return "linux/amd64" if args[3] == "{{.Os}}/{{.Architecture}}" else "sha256:" + "b" * 64
        if args[0] == "create":
            return "container-id"
        if args[0] == "cp":
            Path(args[2]).write_bytes(BINARY)
        return ""

    def test_package_binds_exact_image_source_and_binary(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", self.docker):
            result = stage.package("user", "user:ci", Path(tmp) / "out", self.env)
            self.assertEqual(result["source_sha"], "a" * 40)
            self.assertEqual(result["platform"], "linux/amd64")
            self.assertEqual(result["run_id"], 123)
            out = Path(tmp) / "out"
            digest = hashlib.sha256(BINARY).hexdigest()
            self.assertEqual(result["binary_sha256"], digest)
            self.assertEqual((out / "SHA256SUMS").read_text(), f"{digest}  user\n")
            self.assertEqual(json.loads((out / "manifest.json").read_text()), result)
            self.assertEqual(result["image_id"], "sha256:" + "b" * 64)
            self.assertEqual(result["repository"], "fin-assist/user")
            self.assertEqual(result["run_attempt"], 2)
            self.assertEqual(result["event"], "push")
            self.assertEqual(result["ref"], "refs/heads/main")
            version = re.search(r"github.com/fin-assist/contracts/gen/go\s+(v\S+)", Path("go.mod").read_text()).group(1)
            self.assertEqual(result["modules"]["contracts/gen/go"], version)
            self.assertEqual((out / "user").stat().st_mode & 0o777, 0o755)
            self.assertIn(("cp", "container-id:/user", str(out / "user")), self.calls)
            self.assertIn(("create", "sha256:" + "b" * 64), self.calls)
            self.assertIn(("rm", "-f", "container-id"), self.calls)
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
            self.assertIn(("rm", "-f", "container-id"), self.calls)

    def test_foreign_platform_and_bad_image_id_are_independent(self):
        for image_id, platform in [("sha256:" + "b" * 64, "linux/arm64"), ("sha256:short", "linux/amd64")]:
            self.calls = []
            def inspect(*args):
                self.calls.append(args)
                return platform if args[3] == "{{.Os}}/{{.Architecture}}" else image_id
            with self.subTest(platform=platform), tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", inspect):
                with self.assertRaises(ValueError):
                    stage.package("user", "user:ci", Path(tmp) / "out", self.env)
                self.assertFalse((Path(tmp) / "out").exists())
                self.assertFalse(any(call[0] == "create" for call in self.calls))

    def test_nonregular_missing_and_empty_binary_are_rejected(self):
        for kind in ["symlink", "directory", "missing", "empty"]:
            self.calls = []
            def copy(*args):
                if args[0] != "cp":
                    return self.docker(*args)
                target = Path(args[2])
                if kind == "symlink":
                    real = target.parent / "real"
                    real.write_bytes(b"host-data")
                    target.symlink_to(real)
                elif kind == "directory":
                    target.mkdir()
                elif kind == "empty":
                    target.write_bytes(b"")
                return ""
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", copy):
                out = Path(tmp) / "out"
                with self.assertRaises(ValueError):
                    stage.package("user", "user:ci", out, self.env)
                self.assertFalse((out / "SHA256SUMS").exists())
                self.assertFalse((out / "manifest.json").exists())
                self.assertIn(("rm", "-f", "container-id"), self.calls)

    def test_copy_failure_not_replaced_by_cleanup_failure(self):
        def fail(*args):
            if args[0] in ["cp", "rm"]:
                raise RuntimeError(args[0] + " failed")
            return self.docker(*args)
        with tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", fail), patch("sys.stderr"):
            with self.assertRaisesRegex(RuntimeError, "cp failed"):
                stage.package("user", "user:ci", Path(tmp) / "out", self.env)

    def test_docker_failure_includes_diagnostic(self):
        result = subprocess.CompletedProcess([], 1, "", "No such image")
        with patch.object(stage.subprocess, "run", return_value=result) as run:
            with self.assertRaisesRegex(RuntimeError, "No such image"):
                stage.docker("image", "inspect", "missing")
            self.assertEqual(run.call_args.kwargs["timeout"], 60)

    def test_workflow_extracts_from_smoke_tested_image(self):
        root = Path(__file__).resolve().parents[1]
        common = Path(subprocess.check_output(
            [sys.executable, str(root / "scripts/common_ci.py")], text=True, timeout=180).strip())
        manifest = json.loads((root / ".finassist-ci.json").read_text())
        caller = yaml.safe_load((root / ".github/workflows/ci.yml").read_text())["jobs"]["image-check"]
        module = re.search(r"^module github.com/fin-assist/(\S+)", (root / "go.mod").read_text()).group(1)
        service = "service" if module == "go-service-template" else module
        fake = service == "assistant"
        self.assertEqual(caller["with"]["service"], service)
        self.assertEqual(caller["with"].get("fake_llm", False), fake)
        self.assertEqual(caller["with"]["revision"], manifest["revision"])
        self.assertEqual(caller["uses"], "fin-assist/go-ci/.github/workflows/shared-image.yml@" + manifest["revision"])

        shared = yaml.safe_load((common.parent / ".github/workflows/shared-image.yml").read_text())
        steps = shared["jobs"]["image"]["steps"]
        build_step = next(step for step in steps if step.get("id") == "image")
        extract = next(step for step in steps if step.get("name") == "Extract stage binary from the tested image")
        self.assertIn("common/scripts/image_smoke.py", build_step["run"])
        self.assertLess(steps.index(build_step), steps.index(extract))
        self.assertEqual(extract["env"]["IMAGE_TAG"], "${{ steps.image.outputs.image_tag }}")
        self.assertIn('scripts/stage_artifact.py "$SERVICE" "$IMAGE_TAG"', extract["run"])
        fake_extract = next(step for step in steps if step.get("name") == "Extract stage fake provider from its tested image")
        self.assertEqual(fake_extract["if"], "inputs.fake_llm")
        self.assertLess(steps.index(build_step), steps.index(fake_extract))
        self.assertIn("scripts/stage_artifact.py fake-llm fake-llm:ci", fake_extract["run"])

        spec = importlib.util.spec_from_file_location("shared_image_smoke", common / "scripts/image_smoke.py")
        smoke = importlib.util.module_from_spec(spec)
        with patch.object(sys, "dont_write_bytecode", True):
            spec.loader.exec_module(smoke)
        calls = []
        revision = "a" * 40

        def run(arguments, **options):
            calls.append(arguments)
            if arguments[:2] == ["docker", "port"]:
                return "127.0.0.1:12345"
            if arguments[:2] == ["docker", "logs"]:
                return revision
            if arguments[:2] == ["docker", "inspect"]:
                return "0"
            if arguments[0] == "curl":
                return "404" if "%{http_code}" in arguments else "go_goroutines"
            return "usage: USER_GRPC_ADDR loopback listen address"

        with patch.object(sys, "dont_write_bytecode", True):
            image = smoke.build_images(service, fake, revision, environ={}, run=run)
        expected_image = ("service" if service == "user" else service) + ":ci"
        self.assertEqual(image, expected_image)
        expected = [(service, image)] + ([("fake-llm", "fake-llm:ci")] if fake else [])
        for binary, tag in expected:
            build = next(i for i, args in enumerate(calls) if args[:2] == ["docker", "build"] and tag in args)
            tested = next(i for i, args in enumerate(calls) if args[:3] == ["docker", "run", "--rm"] and tag in args)
            self.assertLess(build, tested)
            dockerfile = root / ("Dockerfile.fake-llm" if binary == "fake-llm" else "Dockerfile")
            self.assertRegex(dockerfile.read_text(), rf"(?m)^COPY --from=build /out/{re.escape(binary)} /{re.escape(binary)}$")
        if service == "bank-bff":
            self.assertTrue(any(args[:2] == ["docker", "stop"] for args in calls))
        # The helper import must not invalidate the next local or CI bootstrap.
        subprocess.run([sys.executable, str(root / "scripts/common_ci.py")], check=True,
                       capture_output=True, text=True, timeout=180)

    def test_existing_directory_not_overwritten(self):
        with tempfile.TemporaryDirectory() as tmp, patch.object(stage, "docker", self.docker):
            with self.assertRaises(FileExistsError):
                stage.package("user", "user:ci", Path(tmp), self.env)
