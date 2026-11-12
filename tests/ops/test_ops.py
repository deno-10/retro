import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("retro_ops", Path(__file__).resolve().parents[2] / "deploy/ops.py")
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)


class OperationsTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.cfg = {"project_dir": self.directory.name, "state_dir": self.directory.name,
                    "backup_host": "test", "alert_after_failures": 3}
        self.service = ops.Operations(self.cfg)

    def test_alert_only_after_repeated_failure_and_recovery(self):
        with patch.object(self.service, "notify") as notify, patch.object(self.service, "conditions", return_value=(["диск заполнен"], {})):
            self.service.check()
            self.service.check()
            notify.assert_not_called()
            self.service.check()
            self.service.check()
            notify.assert_called_once()
        with patch.object(self.service, "notify") as notify, patch.object(self.service, "conditions", return_value=([], {})):
            self.assertTrue(self.service.check())
            self.service.check()
            notify.assert_called_once_with("Retro: работа восстановлена")

    def test_failed_notification_is_retried(self):
        self.cfg["alert_after_failures"] = 1
        with patch.object(self.service, "conditions", return_value=(["сбой"], {})):
            with patch.object(self.service, "notify", side_effect=OSError("недоступен получатель")):
                with self.assertRaises(OSError):
                    self.service.check()
            self.assertEqual(ops.read_json(self.service.state / "monitor.json")["notified"], [])
            with patch.object(self.service, "notify") as notify:
                self.service.check()
                notify.assert_called_once()

    def test_failed_backup_never_updates_success_timestamp(self):
        old = {"at": 123, "snapshot_id": "old"}
        ops.write_json(self.service.state / "backup.json", old)
        with patch.object(self.service, "_backup", side_effect=RuntimeError("restic недоступен")):
            with self.assertRaises(RuntimeError):
                self.service.backup()
        self.assertEqual(ops.read_json(self.service.state / "backup.json"), old)
        self.assertTrue((self.service.state / "backup-failure.json").exists())

    def backup_runner(self, broken_restore=False, broken_verify=False):
        def run(args, *, output=None, restic=False, timeout=1200):
            if "exec" in args:
                output.write("проверенный снимок".encode())
            elif args[:2] == ["restic", "backup"]:
                return b'{"message_type":"summary","snapshot_id":"aabbccdd"}\n'
            elif args[:2] == ["restic", "dump"]:
                self.assertEqual(args[2], "aabbccdd")
                output.write("повреждение".encode() if broken_restore else "проверенный снимок".encode())
            elif "verify" in args and broken_verify:
                raise RuntimeError("ошибка проверки SQLite")
            return b""
        return run

    def test_corrupt_download_or_database_does_not_count_as_backup(self):
        for broken_restore, broken_verify in [(True, False), (False, True)]:
            with self.subTest(restore=broken_restore), patch.object(self.service, "run", side_effect=self.backup_runner(broken_restore, broken_verify)):
                with self.assertRaises(RuntimeError):
                    self.service.backup()
                self.assertFalse((self.service.state / "backup.json").exists())
                self.assertEqual(list((self.service.state / "snapshot").iterdir()), [])

    def test_backup_records_success_only_after_restore_and_retention(self):
        with patch.object(self.service, "run", side_effect=self.backup_runner()) as run:
            self.service.backup()
        self.assertEqual(ops.read_json(self.service.state / "backup.json")["snapshot_id"], "aabbccdd")
        commands = [call.args[0] for call in run.call_args_list]
        self.assertTrue(any("verify" in command for command in commands))
        self.assertEqual(commands[-1], ["restic", "check"])
        self.assertEqual(list((self.service.state / "snapshot").iterdir()), [])

    def test_disk_backup_age_and_http_failures_are_detected(self):
        self.cfg.update(public_url="https://example.org/readyz", min_disk_free_gib=2,
                        min_disk_free_percent=15, max_backup_age_hours=27)
        raw = b"retro_http_errors_total 10\nretro_http_requests_total 50\nretro_http_limited_total 30\n"
        before = {"metrics": {"retro_http_errors_total": 0, "retro_http_requests_total": 0, "retro_http_limited_total": 0}}
        with patch.object(self.service, "run", side_effect=[raw, self.directory.name.encode()]), patch.object(ops.urllib.request, "urlopen", side_effect=OSError()), patch.object(ops.shutil, "disk_usage", return_value=type("Disk", (), {"free": 1, "total": 10})()):
            issues, _ = self.service.conditions(before)
        self.assertIn("нет свежего проверенного бэкапа", issues)
        self.assertIn("мало свободного диска", issues)
        self.assertIn("растёт число HTTP 5xx", issues)
        self.assertIn("много отклонений по лимиту нагрузки", issues)
        self.assertIn("публичный сайт недоступен по HTTPS", issues)

    def test_restic_configuration_overrides_inherited_secrets(self):
        self.cfg.update(restic_repository_file="/etc/retro/repository", restic_password_file="/etc/retro/password")
        result = type("Result", (), {"returncode": 0, "stdout": b""})()
        with patch.dict(os.environ, {"RESTIC_REPOSITORY": "/wrong", "RESTIC_PASSWORD": "wrong", "RESTIC_PASSWORD_COMMAND": "wrong"}), patch.object(ops.subprocess, "run", return_value=result) as run:
            self.service.run(["restic", "check"], restic=True)
        env = run.call_args.kwargs["env"]
        self.assertNotIn("RESTIC_REPOSITORY", env)
        self.assertNotIn("RESTIC_PASSWORD", env)
        self.assertNotIn("RESTIC_PASSWORD_COMMAND", env)
        self.assertEqual(env["RESTIC_REPOSITORY_FILE"], "/etc/retro/repository")


if __name__ == "__main__":
    unittest.main()
