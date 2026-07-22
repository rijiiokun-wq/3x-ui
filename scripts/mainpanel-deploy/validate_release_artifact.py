#!/usr/bin/env python3
"""Fail-closed extraction of the single x-ui binary from a GitHub artifact."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import stat
import tarfile
import tempfile
import zipfile


ARTIFACT_MEMBER = "x-ui-linux-amd64.tar.gz"
BINARY_MEMBER = "x-ui/x-ui"
SHA256_RE = re.compile(r"^sha256:([0-9a-f]{64})$")
MAX_ZIP_BYTES = 150 * 1024 * 1024
MAX_TAR_BYTES = 1024 * 1024 * 1024
MAX_TAR_MEMBERS = 2_000
MIN_BINARY_BYTES = 1024 * 1024
MAX_BINARY_BYTES = 150 * 1024 * 1024


class ValidationError(RuntimeError):
    pass


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def safe_member_name(name: str) -> bool:
    if not name or "\\" in name or "\x00" in name:
        return False
    path = PurePosixPath(name)
    return not path.is_absolute() and all(part not in {"", ".", ".."} for part in path.parts)


def validate_and_extract(archive: Path, expected_digest: str, output: Path) -> dict[str, object]:
    match = SHA256_RE.fullmatch(expected_digest)
    if not match:
        raise ValidationError("invalid_artifact_digest")
    if not archive.is_file() or archive.stat().st_size > MAX_ZIP_BYTES:
        raise ValidationError("invalid_artifact_size")
    actual_archive_sha = sha256_file(archive)
    if actual_archive_sha != match.group(1):
        raise ValidationError("artifact_digest_mismatch")

    output.parent.mkdir(parents=True, exist_ok=True)
    if output.exists():
        raise ValidationError("output_already_exists")

    with zipfile.ZipFile(archive) as outer:
        members = outer.infolist()
        if len(members) != 1 or members[0].filename != ARTIFACT_MEMBER:
            raise ValidationError("unexpected_artifact_members")
        zip_member = members[0]
        mode = zip_member.external_attr >> 16
        if zip_member.is_dir() or (mode and not stat.S_ISREG(mode)):
            raise ValidationError("artifact_member_not_regular")
        if zip_member.file_size > MAX_ZIP_BYTES:
            raise ValidationError("nested_archive_too_large")

        with tempfile.TemporaryDirectory(prefix="mainpanel-artifact-") as temp_dir:
            nested_path = Path(temp_dir) / ARTIFACT_MEMBER
            with outer.open(zip_member) as source, nested_path.open("xb") as target:
                shutil.copyfileobj(source, target, length=1024 * 1024)
                target.flush()
                os.fsync(target.fileno())

            with tarfile.open(nested_path, mode="r:gz") as nested:
                tar_members = nested.getmembers()
                if len(tar_members) > MAX_TAR_MEMBERS:
                    raise ValidationError("too_many_tar_members")
                total_size = 0
                binary = None
                for member in tar_members:
                    if not safe_member_name(member.name):
                        raise ValidationError("unsafe_tar_member")
                    if member.issym() or member.islnk() or member.isdev() or member.isfifo():
                        raise ValidationError("unsafe_tar_member_type")
                    total_size += max(0, member.size)
                    if total_size > MAX_TAR_BYTES:
                        raise ValidationError("tar_too_large")
                    if member.name == BINARY_MEMBER:
                        if binary is not None or not member.isfile():
                            raise ValidationError("binary_member_not_unique_regular")
                        binary = member
                if binary is None:
                    raise ValidationError("binary_member_missing")
                if not MIN_BINARY_BYTES <= binary.size <= MAX_BINARY_BYTES:
                    raise ValidationError("invalid_binary_size")
                source = nested.extractfile(binary)
                if source is None:
                    raise ValidationError("binary_extract_failed")
                with source, output.open("xb") as target:
                    shutil.copyfileobj(source, target, length=1024 * 1024)
                    target.flush()
                    os.fsync(target.fileno())

    os.chmod(output, 0o755)
    with output.open("rb") as binary_file:
        header = binary_file.read(20)
    if len(header) < 20 or header[:4] != b"\x7fELF" or header[4:6] != b"\x02\x01":
        output.unlink(missing_ok=True)
        raise ValidationError("binary_not_elf64_little_endian")
    # e_machine is a little-endian uint16 at offset 18. 62 is AMD x86-64.
    if int.from_bytes(header[18:20], "little") != 62:
        output.unlink(missing_ok=True)
        raise ValidationError("binary_not_x86_64")

    binary_sha = sha256_file(output)
    return {
        "artifact_digest": expected_digest,
        "binary_sha256": binary_sha,
        "binary_size": output.stat().st_size,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--archive", type=Path, required=True)
    parser.add_argument("--expected-digest", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        result = validate_and_extract(args.archive, args.expected_digest, args.output)
    except (OSError, ValueError, zipfile.BadZipFile, tarfile.TarError, ValidationError) as exc:
        print(json.dumps({"ok": False, "reason": str(exc)}, separators=(",", ":")))
        return 1
    print(json.dumps({"ok": True, **result}, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
