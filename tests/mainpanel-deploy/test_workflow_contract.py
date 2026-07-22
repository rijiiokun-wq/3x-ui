from __future__ import annotations

from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[2]


class WorkflowContractTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.workflow = (ROOT / ".github/workflows/mainpanel-production.yml").read_text()
        cls.controller = (ROOT / "scripts/mainpanel-deploy/remote_controller.sh").read_text()
        cls.runner = (ROOT / "scripts/mainpanel-deploy/github_remote_transaction.sh").read_text()

    def test_manual_only_and_serialized(self) -> None:
        self.assertIn("workflow_dispatch:", self.workflow)
        self.assertNotIn("pull_request:", self.workflow)
        self.assertNotIn("\n  push:", self.workflow)
        self.assertIn("group: mainpanel-production", self.workflow)
        self.assertIn("cancel-in-progress: false", self.workflow)

    def test_production_jobs_use_environment_and_no_inline_secret_values(self) -> None:
        self.assertEqual(self.workflow.count("environment: mainpanel-production"), 2)
        for secret in (
            "MAINPANEL_HOST",
            "MAINPANEL_USER",
            "MAINPANEL_SSH_KEY",
            "MAINPANEL_KNOWN_HOSTS",
            "MAINPANEL_HEALTH_TOKEN",
        ):
            self.assertIn(f"secrets.{secret}", self.workflow)
        self.assertNotIn("45.67.", self.workflow)
        self.assertNotIn("127.0.0.1:5056", self.workflow)

    def test_exact_sha_and_artifact_provenance_are_required(self) -> None:
        for marker in (
            'event == "push"',
            'head_branch == "maintenance/v3.3"',
            "source_sha_is_not_current_maintenance_head",
            "x-ui-linux-amd64",
            "artifact_digest_mismatch",
        ):
            files = self.workflow + (ROOT / "scripts/mainpanel-deploy/github_release_preflight.sh").read_text() + (ROOT / "scripts/mainpanel-deploy/validate_release_artifact.py").read_text()
            self.assertIn(marker, files)

    def test_controller_has_backup_health_and_binary_first_rollback(self) -> None:
        for marker in (
            ".backup",
            "atomic_install_binary",
            "restore_database_if_schema_changed",
            "restore_binary_and_health",
            'xray.get("state") == "running"',
            "repairClientTrafficCycles",
            "deployment_lock_busy",
        ):
            self.assertIn(marker, self.controller)

    def test_controller_uses_token_file_and_never_reads_secret_from_stdin(self) -> None:
        for marker in (
            "--token-file",
            "missing_token_file_argument",
            "invalid_token_file_path",
            "unsafe_token_file_permissions",
            "rm -f \"$token_file\"",
        ):
            self.assertIn(marker, self.controller)

        # stdin belongs to systemd-run/the SSH transport and must never be the
        # transport for the health token.
        self.assertNotRegex(self.controller, r"read\s+-r\s+health_token\s*$")
        self.assertRegex(
            self.controller,
            r"read\s+-r\s+health_token\s*<\s*\"\$token_file\"",
        )

    def test_workflow_launches_controller_as_durable_transient_unit(self) -> None:
        helper = "scripts/mainpanel-deploy/github_remote_transaction.sh"
        self.assertEqual(self.workflow.count(helper), 2)
        workflow_commands = self._continued_shell_commands(self.workflow)
        helper_commands = [command for command in workflow_commands if helper in command]
        self.assertEqual(len(helper_commands), 2)
        self.assertTrue(any(re.search(rf"{re.escape(helper)}\s+deploy\b", command) for command in helper_commands))
        self.assertTrue(any(re.search(rf"{re.escape(helper)}\s+rollback-binary\b", command) for command in helper_commands))

        self.assertEqual(self.runner.count("systemd-run"), 1)
        self.assertGreaterEqual(self.runner.count("--token-file"), 1)
        self.assertGreaterEqual(self.runner.count("health-token"), 1)
        for marker in (
            "--no-block",
            "StandardInput=null",
            "RemainAfterExit=yes",
            "RuntimeMaxSec",
            "TimeoutStartSec",
            "TimeoutStopSec",
            "StandardOutput=append:",
            "StandardError=append:",
            'remote_exec "${unit_command[@]}"',
            "systemctl show",
        ):
            self.assertIn(marker, self.runner)

        # A broken SSH connection must not terminate the controller. In
        # particular, neither a token pipe nor a foreground bash-over-SSH
        # invocation is allowed.
        launch_files = self.workflow + "\n" + self.runner
        self.assertNotRegex(
            launch_files,
            r"(?m)^\s*printf\s+.*MAINPANEL_HEALTH_TOKEN.*\|\s*ssh\b",
        )
        self.assertNotRegex(
            launch_files,
            r"(?m)^\s*ssh\b[^\n]*(?:/bin/)?bash\b[^\n]*remote_controller\.sh",
        )

    def test_global_exit_and_signal_handlers_roll_back_after_mutation_starts(self) -> None:
        for marker in (
            "trap on_exit EXIT",
            "trap 'failure_reason=signal_hup; exit 129' HUP",
            "trap 'failure_reason=signal_interrupt; exit 130' INT",
            "trap 'failure_reason=signal_terminate; exit 143' TERM",
            "if [[ \"$rollback_armed\" == 'true' ]]",
            "restore_binary_and_health \"$recovery_backup_ref\" \"$health_token\"",
        ):
            self.assertIn(marker, self.controller)

        deploy_body = self._shell_function("deploy")
        rollback_body = self._shell_function("rollback_binary")
        for body in (deploy_body, rollback_body):
            armed = body.index("rollback_armed='true'")
            stopped = body.index('systemctl stop "$service_name"')
            installed = body.index("atomic_install_binary")
            self.assertLess(armed, stopped)
            self.assertLess(armed, installed)

    def test_health_scratch_is_cleaned_by_normal_and_global_exit_paths(self) -> None:
        cleanup_body = self._shell_function("cleanup_health_scratch")
        health_body = self._shell_function("health_check")
        exit_body = self._shell_function("on_exit")
        scratch_match = re.search(
            r"rm\s+-rf\s+(?:--\s+)?\"\$(?P<name>[a-z_][a-z0-9_]*)\"",
            cleanup_body,
        )
        self.assertIsNotNone(scratch_match, cleanup_body)
        assert scratch_match is not None
        scratch_name = scratch_match.group("name")
        self.assertRegex(self.controller, rf"(?m)^{scratch_name}=(?:''|\"\")$")
        self.assertIn(f"{scratch_name}=", health_body)
        self.assertGreaterEqual(health_body.count("cleanup_health_scratch"), 2)
        self.assertGreaterEqual(exit_body.count("cleanup_health_scratch"), 1)
        self.assertIn(f"{scratch_name}=''", cleanup_body)

    def test_controller_and_runner_enforce_exactly_one_result_record(self) -> None:
        emit_body = self._shell_function("emit_result")
        self.assertRegex(
            emit_body,
            r"(?:\[\[\s*\"\$result_emitted\"\s*==\s*'true'\s*\]\]|"
            r"\[\[\s*\"\$result_emitted\"\s*!=\s*'true'\s*\]\]\s*\|\||"
            r"\[\[\s*\"\$result_emitted\"\s*==\s*'false'\s*\]\]\s*\|\|)",
        )
        self.assertIn("result_emitted='true'", emit_body)
        self.assertLess(emit_body.index("result_emitted='true'"), emit_body.index("printf"))

        result_lines = [
            line.strip()
            for line in self.controller.splitlines()
            if "RESULT status=" in line
        ]
        self.assertGreaterEqual(len(result_lines), 4)
        self.assertTrue(
            all(line.startswith("emit_result ") for line in result_lines),
            result_lines,
        )

        # Success is accepted only when the remote result file contains one
        # and only one RESULT record; `tail -n1` would hide duplicates.
        result_count_checks = re.findall(
            r"grep\s+-[A-Za-z]*c[A-Za-z]*\s+[^\n]*\^RESULT",
            self.runner,
        )
        self.assertGreaterEqual(len(result_count_checks), 1)
        self.assertGreaterEqual(len(re.findall(r"result_count[^\n]*(?:==|-eq)\s*(?:'1'|1)", self.runner)), 1)
        self.assertNotRegex(self.workflow + "\n" + self.runner, r"grep[^\n]*\^RESULT[^\n]*\|\s*tail\s+-n\s*1")

    def _shell_function(self, name: str) -> str:
        match = re.search(
            rf"(?ms)^{re.escape(name)}\(\) \{{\n(?P<body>.*?)^\}}$",
            self.controller,
        )
        self.assertIsNotNone(match, f"shell function {name!r} not found")
        assert match is not None
        return match.group("body")

    @staticmethod
    def _continued_shell_commands(text: str) -> list[str]:
        commands: list[str] = []
        current = ""
        for raw_line in text.splitlines():
            line = raw_line.strip()
            if current:
                current += " " + line
            else:
                current = line
            if current.endswith("\\"):
                current = current[:-1].rstrip()
                continue
            commands.append(current)
            current = ""
        if current:
            commands.append(current)
        return commands


if __name__ == "__main__":
    unittest.main()
