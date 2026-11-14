import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[2] / "deploy/set-team-password.py"
spec = importlib.util.spec_from_file_location("team_password", SCRIPT)
passwords = importlib.util.module_from_spec(spec)
spec.loader.exec_module(passwords)


class SharedPasswordTests(unittest.TestCase):
    def test_only_hash_is_published_and_failure_preserves_previous_credentials(self):
        synthetic = "only-for-this-test-1234"
        hashed = b"$2a$12$" + 53 * b"a"
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "users.caddy"
            with patch.object(subprocess, "run", return_value=subprocess.CompletedProcess([], 0, hashed)) as run:
                passwords.set_password(target, synthetic)
                self.assertNotIn(synthetic, str(run.call_args.args))
                self.assertEqual(run.call_args.kwargs["input"], synthetic.encode() + b"\n")
                self.assertTrue(run.call_args.kwargs["capture_output"])
            expected = b"team " + hashed + b"\n"
            self.assertEqual(target.read_bytes(), expected)
            self.assertEqual(os.stat(target).st_mode & 0o777, 0o600)
            with patch.object(subprocess, "run", return_value=subprocess.CompletedProcess([], 0, synthetic.encode())):
                with self.assertRaises(ValueError):
                    passwords.set_password(target, synthetic)
            with self.assertRaises(ValueError):
                passwords.set_password(target, "short")
            self.assertEqual(list(target.parent.iterdir()), [target])
            self.assertEqual(target.read_bytes(), expected)
