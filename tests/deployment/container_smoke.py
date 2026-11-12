"""Проверка настоящего Compose-стека в CI. Не использует рабочие тома."""
import json
import os
from pathlib import Path
import shutil
import ssl
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[2]


def run(*args, **kwargs):
    return subprocess.run(args, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, **kwargs).stdout


def main():
    if os.environ.get("RETRO_CONTAINER_SMOKE") != "1":
        raise RuntimeError("контейнерный тест запускается явно: RETRO_CONTAINER_SMOKE=1")
    name = "retro-ci-" + uuid.uuid4().hex[:12]
    with tempfile.TemporaryDirectory(prefix=name) as directory:
        project = Path(directory)
        (project / "deploy/secrets").mkdir(parents=True)
        shutil.copy(ROOT / "compose.yaml", project / "compose.yaml")
        config = (ROOT / "deploy/Caddyfile").read_text().replace("admin off", "admin off\n\tskip_install_trust")
        (project / "deploy/Caddyfile").write_text(config)
        (project / ".env").write_text(f"RETRO_DOMAIN=localhost\nACME_EMAIL=test@example.org\nRETRO_IMAGE=retro:ci\nRETRO_DATA_VOLUME={name}-data\n")
        hashed = run("docker", "run", "--rm", "caddy:2.11.9-alpine", "caddy", "hash-password", "--plaintext", "test-only").decode().strip()
        (project / "deploy/secrets/users.caddy").write_text("team " + hashed + "\n")
        compose = ["docker", "compose", "--project-name", name, "--project-directory", directory, "-f", str(project / "compose.yaml")]
        context = ssl._create_unverified_context()

        def request(path, method="GET", data=None):
            req = urllib.request.Request("https://localhost" + path, method=method,
                data=json.dumps(data).encode() if data is not None else None,
                headers={"Authorization": "Basic dGVhbTp0ZXN0LW9ubHk=", "X-Participant": "owner-00000001", "Content-Type": "application/json"})
            with urllib.request.urlopen(req, context=context, timeout=3) as response:
                return json.load(response)

        def ready():
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                try:
                    if request("/readyz")["status"] == "ready":
                        return
                except Exception:
                    pass
                time.sleep(.3)
            raise RuntimeError("Compose не готов")

        try:
            run(*compose, "config", "--quiet")
            run(*compose, "up", "-d", "--no-build", "--wait", "--wait-timeout", "60")
            ready()
            board = request("/api/boards", "POST", {"title": "Перезапуск контейнера"})
            run(*compose, "restart", "retro")
            ready()
            assert request("/api/boards/" + board["id"])["isOwner"]
            container = run(*compose, "ps", "-q", "retro").decode().strip()
            before = int(run("docker", "inspect", "--format", "{{.RestartCount}}", container))
            # Завершение приложения внутри контейнера проверяет restart policy,
            # а не явный docker stop, который намеренно отключает перезапуск.
            subprocess.run(compose + ["exec", "-T", "retro", "sh", "-c", "kill -KILL $(pidof retro)"], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            deadline = time.monotonic() + 30
            while int(run("docker", "inspect", "--format", "{{.RestartCount}}", container)) <= before:
                if time.monotonic() > deadline:
                    raise RuntimeError("restart policy не восстановила процесс")
                time.sleep(.3)
            ready()
            assert request("/api/boards/" + board["id"])["title"] == board["title"]
            # У рабочего контейнера нет доступного с хоста порта 8080.
            bindings = json.loads(run("docker", "inspect", "--format", "{{json .HostConfig.PortBindings}}", container))
            assert not bindings
            print("Compose: HTTPS, вход, здоровье, постоянный том и автоперезапуск проверены")
        except Exception:
            print(run(*compose, "logs", "--tail", "100").decode())
            raise
        finally:
            run(*compose, "down", "--volumes", "--remove-orphans")


if __name__ == "__main__":
    main()
