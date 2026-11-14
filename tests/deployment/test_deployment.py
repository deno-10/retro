"""Настоящие процессы Retro/Caddy и restic; только временные порты и данные."""
import base64
import http.client
import importlib.util
import json
import os
from pathlib import Path
import shutil
import socket
import ssl
import subprocess
import tempfile
import time
import unittest
import urllib.error
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("retro_ops", ROOT / "deploy/ops.py")
ops = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ops)


def unused_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class DeploymentTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.binary = os.environ["RETRO_TEST_BINARY"]
        cls.caddy_binary = os.environ["CADDY_TEST_BINARY"]

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="retro-deployment-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.backend_port, self.port = unused_port(), unused_port()
        self.log = (self.directory / "processes.log").open("w+")
        self.addCleanup(self.log.close)
        self.processes = []
        self.addCleanup(self.stop_processes)
        self.backend = self.start_backend()
        self.tls = ssl._create_unverified_context()  # только временный localhost CA
        password = "temporary-test-password"
        hashed = subprocess.check_output([self.caddy_binary, "hash-password", "--plaintext", password], text=True).strip()
        users = self.directory / "users.caddy"
        users.write_text("team " + hashed + "\n")
        config = (ROOT / "deploy/Caddyfile").read_text().replace(
            "admin off", "admin off\n\tskip_install_trust\n\tauto_https disable_redirects")
        config = config.replace("import /run/secrets/retro_users", f"import {users}")
        config = config.replace("retro:8080", f"127.0.0.1:{self.backend_port}")
        config = config.replace("{$RETRO_DOMAIN} {", "{$RETRO_DOMAIN} {\n\tbind 127.0.0.1")
        caddyfile = self.directory / "Caddyfile"
        caddyfile.write_text(config)
        env = dict(os.environ, RETRO_DOMAIN=f"localhost:{self.port}", ACME_EMAIL="test@example.org",
                   XDG_DATA_HOME=str(self.directory / "caddy-data"), XDG_CONFIG_HOME=str(self.directory / "caddy-config"))
        subprocess.run([self.caddy_binary, "validate", "--config", str(caddyfile), "--adapter", "caddyfile"], env=env, stdout=self.log, stderr=self.log, check=True)
        self.processes.append(subprocess.Popen([self.caddy_binary, "run", "--config", str(caddyfile), "--adapter", "caddyfile"], env=env, stdout=self.log, stderr=self.log))
        self.authorization = "Basic " + base64.b64encode(("team:" + password).encode()).decode()
        self.wait_ready()
        self.cookie = self.request("GET", "/")[2]["Set-Cookie"].split(";", 1)[0]

    def start_backend(self):
        process = subprocess.Popen([self.binary, "-addr", f"127.0.0.1:{self.backend_port}", "-data", str(self.directory / "retro.db"), "-trusted-proxy", "127.0.0.1/32"], stdout=self.log, stderr=self.log)
        self.processes.append(process)
        return process

    def stop_processes(self):
        for process in reversed(self.processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=8)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=3)

    def request(self, method, path, data=None, auth=True, extra=None):
        headers = {"Content-Type": "application/json"}
        if getattr(self, "cookie", None):
            headers["Cookie"] = self.cookie
        if auth:
            headers["Authorization"] = self.authorization
        headers.update(extra or {})
        request = urllib.request.Request(f"https://localhost:{self.port}" + path,
                                         data=json.dumps(data).encode() if data is not None else None,
                                         headers=headers, method=method)
        try:
            response = urllib.request.urlopen(request, context=self.tls, timeout=5)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            return response.status, raw, response.headers

    def wait_ready(self):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            try:
                if self.request("GET", "/readyz", auth=False)[0] == 200:
                    return
            except Exception:
                pass
            time.sleep(.05)
        self.log.flush()
        self.fail("Не поднялся тестовый сервис: " + (self.directory / "processes.log").read_text()[-5000:])

    def create_board(self):
        status, raw, _ = self.request("POST", "/api/boards", {"title": "Постоянная работа"})
        self.assertEqual(status, 201)
        return json.loads(raw)

    def test_https_access_control_and_csrf(self):
        board = self.create_board()
        path = "/api/boards/" + board["id"]
        foreign_cookie = self.request("GET", "/", extra={"Cookie": ""})[2]["Set-Cookie"].split(";", 1)[0]
        for target in ("/b/" + board["id"], path, path + "/history", path + "/export.md", path + "/events"):
            status, raw, headers = self.request("GET", target, extra={"Cookie": foreign_cookie})
            self.assertEqual(status, 404, target)
            self.assertNotIn(board["title"].encode(), raw)
            self.assertNotIn("Content-Disposition", headers)
        for path in ("/", "/static/app.js", "/api/templates", "/api/boards/" + board["id"],
                     f'/api/boards/{board["id"]}/history', f'/api/boards/{board["id"]}/export.md',
                     f'/api/boards/{board["id"]}/events'):
            self.assertEqual(self.request("GET", path, auth=False)[0], 401, path)
        self.assertEqual(self.request("GET", "/", extra={"Authorization": "Basic d3Jvbmc6d3Jvbmc="})[0], 401)
        self.assertEqual(self.request("GET", "/metrics")[0], 404)
        self.assertEqual(self.request("GET", "/healthz")[0], 404)
        self.assertEqual(self.request("POST", "/api/boards", {"title": "Не создать"}, extra={"Origin": "https://evil.example"})[0], 403)
        self.assertEqual(self.request("POST", "/api/boards", {"title": "Неверная схема"}, extra={"Origin": f"http://localhost:{self.port}"})[0], 403)
        self.assertEqual(self.request("POST", "/api/boards", {"title": "Запрос из браузера"}, extra={"Origin": f"https://localhost:{self.port}"})[0], 201)
        status, _, headers = self.request("GET", "/")
        self.assertEqual(status, 200)
        self.assertIn("max-age", headers["Strict-Transport-Security"])
        self.assertIn("frame-ancestors 'none'", headers["Content-Security-Policy"])

    def test_sse_through_https_and_restart_preserves_data(self):
        board = self.create_board()
        path = f'/api/boards/{board["id"]}'
        connection = http.client.HTTPSConnection("localhost", self.port, context=self.tls, timeout=5)
        self.addCleanup(connection.close)
        connection.request("GET", path + "/events", headers={"Authorization": self.authorization, "Cookie": self.cookie})
        response = connection.getresponse()
        self.assertEqual(response.status, 200)

        def next_board():
            while True:
                line = response.readline()
                if not line:
                    self.fail("SSE закрылся до получения данных")
                if line.startswith(b"data: "):
                    return json.loads(line[6:])

        self.assertEqual(next_board()["id"], board["id"])
        body = {"columnId": board["columns"][0]["id"], "text": "Сохранить после перезапуска"}
        status, _, _ = self.request("POST", path + "/cards", body, extra={"Idempotency-Key": "durable-card-key"})
        self.assertEqual(status, 200)
        current = next_board()
        self.assertEqual(current["columns"][0]["cards"][0]["text"], body["text"])
        # Открытый поток не должен задерживать корректную остановку.
        self.backend.terminate()
        self.assertEqual(self.backend.wait(timeout=7), 0)
        connection.close()
        self.backend = self.start_backend()
        self.wait_ready()
        status, raw, _ = self.request("POST", path + "/cards", body, extra={"Idempotency-Key": "durable-card-key"})
        self.assertEqual(status, 200)
        self.assertEqual(len(json.loads(raw)["columns"][0]["cards"]), 1)

    def test_real_restic_round_trip_from_live_database(self):
        board = self.create_board()
        cfg = {"project_dir": str(ROOT), "state_dir": str(self.directory / "ops"), "backup_host": "test",
               "restic_repository_file": str(self.directory / "repository"), "restic_password_file": str(self.directory / "password")}
        Path(cfg["restic_repository_file"]).write_text(str(self.directory / "restic-repo"))
        Path(cfg["restic_password_file"]).write_text("temporary-restic-test-password")
        binary, source = self.binary, self.directory / "retro.db"

        class NativeOperations(ops.Operations):
            # Вместо docker exec запускаем тот же CLI на временной нативной БД.
            # Шифрование, загрузка, скачивание, retention и check — настоящий restic.
            def run(self, args, *, output=None, restic=False, timeout=1200):
                if "exec" in args:
                    args = [binary, "backup", "-data", str(source), "-out", "-"]
                elif "verify" in args:
                    args = [binary, "verify", "-data", str(self.state / "snapshot/restored.db")]
                return super().run(args, output=output, restic=restic, timeout=timeout)

        service = NativeOperations(cfg)
        service.run(["restic", "init"], restic=True)
        service.backup()
        saved = ops.read_json(service.state / "backup.json")
        restored = self.directory / "restored.db"
        with restored.open("wb") as output:
            service.run(["restic", "dump", saved["snapshot_id"], saved["path"]], output=output, restic=True)
        subprocess.run([self.binary, "verify", "-data", str(restored)], check=True)
        # Непосредственно читаем восстановленную базу, в том числе владельца.
        import sqlite3
        with sqlite3.connect(restored) as db:
            data = json.loads(db.execute("SELECT data FROM boards WHERE id = ?", (board["id"],)).fetchone()[0])
        self.assertEqual(data["ownerId"], self.cookie.split("=", 1)[1].split(".", 1)[0])
        self.assertEqual(data["title"], board["title"])


if __name__ == "__main__":
    unittest.main()
