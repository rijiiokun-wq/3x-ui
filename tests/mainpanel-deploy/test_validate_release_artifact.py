from __future__ import annotations

import hashlib
import importlib.util
import io
from pathlib import Path
import stat
import tarfile
import tempfile
import unittest
import zipfile


ROOT = Path(__file__).resolve().parents[2]
MODULE_PATH = ROOT / "scripts/mainpanel-deploy/validate_release_artifact.py"
SPEC = importlib.util.spec_from_file_location("validate_release_artifact", MODULE_PATH)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def fake_amd64_elf() -> bytes:
    header = bytearray(20)
    header[:4] = b"\x7fELF"
    header[4:6] = b"\x02\x01"
    header[18:20] = (62).to_bytes(2, "little")
    return bytes(header) + b"\0" * (1024 * 1024)


def make_artifact(path: Path, *, member_name: str = "x-ui/x-ui", kind: str = "file") -> str:
    nested = io.BytesIO()
    with tarfile.open(fileobj=nested, mode="w:gz") as tar:
        info = tarfile.TarInfo(member_name)
        if kind == "symlink":
            info.type = tarfile.SYMTYPE
            info.linkname = "/tmp/escape"
            info.size = 0
            tar.addfile(info)
        else:
            payload = fake_amd64_elf()
            info.mode = 0o755
            info.size = len(payload)
            tar.addfile(info, io.BytesIO(payload))
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as outer:
        info = zipfile.ZipInfo("x-ui-linux-amd64.tar.gz")
        info.external_attr = (stat.S_IFREG | 0o600) << 16
        outer.writestr(info, nested.getvalue())
    return "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()


class ValidateArtifactTests(unittest.TestCase):
    def test_extracts_only_expected_regular_amd64_binary(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            archive = Path(temp) / "artifact.zip"
            output = Path(temp) / "x-ui"
            digest = make_artifact(archive)
            result = MODULE.validate_and_extract(archive, digest, output)
            self.assertEqual(result["binary_sha256"], hashlib.sha256(output.read_bytes()).hexdigest())
            self.assertTrue(output.stat().st_mode & stat.S_IXUSR)

    def test_rejects_wrong_digest(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            archive = Path(temp) / "artifact.zip"
            make_artifact(archive)
            with self.assertRaisesRegex(MODULE.ValidationError, "artifact_digest_mismatch"):
                MODULE.validate_and_extract(archive, "sha256:" + "0" * 64, Path(temp) / "x-ui")

    def test_rejects_traversal_member(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            archive = Path(temp) / "artifact.zip"
            digest = make_artifact(archive, member_name="../x-ui/x-ui")
            with self.assertRaisesRegex(MODULE.ValidationError, "unsafe_tar_member"):
                MODULE.validate_and_extract(archive, digest, Path(temp) / "x-ui")

    def test_rejects_symlink_binary(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            archive = Path(temp) / "artifact.zip"
            digest = make_artifact(archive, kind="symlink")
            with self.assertRaisesRegex(MODULE.ValidationError, "unsafe_tar_member_type"):
                MODULE.validate_and_extract(archive, digest, Path(temp) / "x-ui")


if __name__ == "__main__":
    unittest.main()
