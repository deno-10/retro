#!/usr/bin/env python3
"""Эксплуатация одного Retro на Linux: только стандартная библиотека Python."""

import argparse
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import time
import urllib.request


def read_json(path, default=None):
    try:
        return json.loads(Path(path).read_text())
    except FileNotFoundError:
        if default is not None:
            return default
        raise


def write_json(path, value):
    path = Path(path)
    temporary = path.with_suffix(".tmp")
    with temporary.open("w") as output:
        json.dump(value, output)
        output.flush()
        os.fsync(output.fileno())
    temporary.replace(path)


def load_config(path):
    cfg = read_json(path)
    for key in ("project_dir", "state_dir", "restic_repository_file", "restic_password_file", "webhook_url_file"):
        if not Path(cfg[key]).is_absolute():
            raise ValueError(f"{key}: необходим абсолютный путь")
    if not cfg["public_url"].startswith("https://"):
        raise ValueError("public_url должен использовать HTTPS")
    repository = Path(cfg["restic_repository_file"]).read_text().strip()
    if not repository.startswith(("s3:", "sftp:", "rest:https://", "azure:", "gs:", "b2:", "rclone:")):
        raise ValueError("бэкап должен находиться во внешнем restic-хранилище")
    if not Path(cfg["restic_password_file"]).read_text().strip():
        raise ValueError("пустой пароль restic")
    webhook = Path(cfg["webhook_url_file"]).read_text().strip()
    if not webhook.startswith("https://"):
        raise ValueError("webhook должен использовать HTTPS")
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,80}", cfg["backup_host"]):
        raise ValueError("некорректный backup_host")
    for key, default in (("max_backup_age_hours", 27), ("min_disk_free_gib", 2), ("min_disk_free_percent", 15), ("alert_after_failures", 3)):
        cfg[key] = float(cfg.get(key, default))
        if cfg[key] <= 0:
            raise ValueError(f"{key}: ожидается положительное число")
    return cfg


class Operations:
    def __init__(self, cfg):
        self.cfg = cfg
        self.state = Path(cfg["state_dir"])
        self.state.mkdir(mode=0o700, parents=True, exist_ok=True)
        self.compose = ["docker", "compose", "--project-directory", cfg["project_dir"],
                        "--env-file", str(Path(cfg["project_dir"]) / ".env"),
                        "-f", str(Path(cfg["project_dir"]) / "compose.yaml")]

    def run(self, args, *, output=None, restic=False, timeout=1200):
        env = os.environ.copy()
        if restic:
            # Унаследованный RESTIC_REPOSITORY/PASSWORD не должен незаметно
            # переопределять выбранное хранилище и пароль из файлов.
            for key in ("RESTIC_REPOSITORY", "RESTIC_PASSWORD", "RESTIC_PASSWORD_COMMAND"):
                env.pop(key, None)
            if self.cfg.get("restic_environment_file"):
                extra = read_json(self.cfg["restic_environment_file"])
                # Только параметры хранилища, без PATH/LD_PRELOAD и команд.
                for key, value in extra.items():
                    if not key.startswith(("AWS_", "B2_", "AZURE_", "GOOGLE_", "RESTIC_REST_")) or not isinstance(value, str):
                        raise ValueError("неподдерживаемый параметр restic_environment_file")
                env.update(extra)
            env.update(RESTIC_REPOSITORY_FILE=self.cfg["restic_repository_file"],
                       RESTIC_PASSWORD_FILE=self.cfg["restic_password_file"],
                       RESTIC_CACHE_DIR=str(self.state / "cache"))
        result = subprocess.run(args, env=env, cwd=self.cfg["project_dir"],
                                stdout=output or subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
        if result.returncode:
            # stderr внешней программы может содержать URL с секретом.
            raise RuntimeError(f"{args[0]} завершился с кодом {result.returncode}; проверьте доступ и конфигурацию")
        return result.stdout

    @contextlib.contextmanager
    def lock(self, name):
        with (self.state / (name + ".lock")).open("a") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            yield

    def backup(self):
        with self.lock("backup"):
            try:
                self._backup()
            except Exception:
                write_json(self.state / "backup-failure.json", {"at": time.time()})
                raise

    def _backup(self):
        stage = self.state / "snapshot"
        stage.mkdir(mode=0o700, exist_ok=True)
        source, restored = stage / "retro.db", stage / "restored.db"
        partial = stage / "retro.db.tmp"
        try:
            with partial.open("wb") as output:
                self.run(self.compose + ["exec", "-T", "retro", "/app/retro", "backup", "-data", "/app/data/retro.db", "-out", "-"], output=output)
                output.flush()
                os.fsync(output.fileno())
            partial.replace(source)
            log = self.run(["restic", "backup", str(source), "--host", self.cfg["backup_host"], "--tag", "retro", "--json"], restic=True)
            records = [json.loads(line) for line in log.splitlines() if line.strip()]
            snapshot = next(row["snapshot_id"] for row in records if row.get("message_type") == "summary")
            if not re.fullmatch(r"[0-9a-f]{8,64}", snapshot):
                raise ValueError("restic не вернул ID сохранённого снимка")
            # Проверяем именно только что загруженный снимок, не чужой latest.
            with restored.open("wb") as output:
                self.run(["restic", "dump", snapshot, str(source)], output=output, restic=True)
            if file_hash(source) != file_hash(restored):
                raise RuntimeError("восстановленная копия отличается от исходной")
            self.run(self.compose + ["run", "--rm", "--no-deps", "-T", "--user", "0:0",
                                    "--volume", f"{stage}:/restore:ro", "retro", "verify", "-data", "/restore/restored.db"])
            self.run(["restic", "forget", "--host", self.cfg["backup_host"], "--tag", "retro",
                      "--group-by", "host,tags", "--keep-daily", "7", "--keep-weekly", "4", "--keep-monthly", "6", "--prune"], restic=True)
            self.run(["restic", "check"], restic=True)
            write_json(self.state / "backup.json", {"at": time.time(), "snapshot_id": snapshot, "path": str(source)})
            (self.state / "backup-failure.json").unlink(missing_ok=True)
        finally:
            partial.unlink(missing_ok=True)
            restored.unlink(missing_ok=True)
            source.unlink(missing_ok=True)

    def conditions(self, previous):
        issues = []
        try:
            with urllib.request.urlopen(self.cfg["public_url"], timeout=8) as response:
                if response.status != 200 or json.loads(response.read(4096)).get("status") != "ready":
                    issues.append("публичный сайт не готов")
        except Exception:
            issues.append("публичный сайт недоступен по HTTPS")
        metrics = {}
        try:
            raw = self.run(self.compose + ["exec", "-T", "retro", "wget", "-q", "-O", "-", "http://127.0.0.1:8080/metrics"], timeout=8)
            metrics = {line.split()[0]: float(line.split()[1]) for line in raw.decode().splitlines() if line and not line.startswith("#")}
            before = previous.get("metrics", {})
            errors = metrics["retro_http_errors_total"] - before.get("retro_http_errors_total", metrics["retro_http_errors_total"])
            requests = metrics["retro_http_requests_total"] - before.get("retro_http_requests_total", metrics["retro_http_requests_total"])
            if errors >= 5 or (requests >= 10 and errors / requests >= .1):
                issues.append("растёт число HTTP 5xx")
            limited = metrics["retro_http_limited_total"] - before.get("retro_http_limited_total", metrics["retro_http_limited_total"])
            if limited >= 20:
                issues.append("много отклонений по лимиту нагрузки")
        except Exception:
            issues.append("метрики приложения недоступны")
        try:
            docker_root = self.run(["docker", "info", "--format", "{{.DockerRootDir}}"], timeout=5).decode().strip()
            for path in {docker_root, str(self.state)}:
                usage = shutil.disk_usage(path)
                if usage.free < self.cfg["min_disk_free_gib"] * 1024**3 or usage.free / usage.total * 100 < self.cfg["min_disk_free_percent"]:
                    issues.append("мало свободного диска")
                    break
        except Exception:
            issues.append("не удалось проверить диск Docker")
        try:
            backup = read_json(self.state / "backup.json", {})
            age = time.time() - backup.get("at", 0)
            if age < -300 or age > self.cfg["max_backup_age_hours"] * 3600:
                issues.append("нет свежего проверенного бэкапа")
            if (self.state / "backup-failure.json").exists():
                issues.append("последняя попытка бэкапа завершилась ошибкой")
        except Exception:
            issues.append("статус бэкапа повреждён")
        return sorted(set(issues)), metrics

    def notify(self, text):
        url = Path(self.cfg["webhook_url_file"]).read_text().strip()
        request = urllib.request.Request(url, data=json.dumps({"text": text}, ensure_ascii=False).encode(),
                                         headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(request, timeout=8) as response:
            if not 200 <= response.status < 300:
                raise RuntimeError("получатель не подтвердил уведомление")

    def check(self):
        with self.lock("monitor"):
            previous = read_json(self.state / "monitor.json", {})
            issues, metrics = self.conditions(previous)
            failures = previous.get("failures", 0) + 1 if issues else 0
            notified = previous.get("notified", [])
            should_notify = (issues and failures >= self.cfg["alert_after_failures"] and issues != notified) or (not issues and notified)
            state = {"failures": failures, "metrics": metrics, "notified": notified, "at": time.time(), "issues": issues}
            try:
                if should_notify:
                    self.notify("Retro: " + ("; ".join(issues) if issues else "работа восстановлена"))
                    state["notified"] = issues
            finally:
                # При ошибке доставки не запоминаем её как успешную: следующая
                # проверка повторит отправку, сохранив накопленный счётчик.
                write_json(self.state / "monitor.json", state)
            return not issues


def file_hash(path):
    digest = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.digest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", default="/etc/retro/ops.json")
    parser.add_argument("command", choices=("init", "backup", "check", "test-alert"))
    args = parser.parse_args()
    os.umask(0o077)
    try:
        ops = Operations(load_config(args.config))
        if args.command == "init":
            ops.run(["restic", "init"], restic=True)
        elif args.command == "backup":
            ops.backup()
        elif args.command == "test-alert":
            ops.notify("Retro: проверка доставки уведомлений")
        elif not ops.check():
            print("Retro: обнаружена проблема; состояние записано в monitor.json", file=sys.stderr)
            return 1
        return 0
    except Exception as error:
        # Не печатаем URL/тело HTTP-ошибки, где может находиться секрет webhook.
        detail = str(error) if isinstance(error, (ValueError, RuntimeError)) else type(error).__name__
        print(f"Retro: операция {args.command} не завершена ({detail})", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
