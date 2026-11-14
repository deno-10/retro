#!/usr/bin/env python3
"""Записывает только bcrypt-хеш общего пароля Caddy."""
import argparse
import getpass
import hmac
import os
from pathlib import Path
import re
import subprocess
import tempfile
import sys


def set_password(target, password, caddy="caddy"):
    raw = password.encode()
    if len(password) < 16 or len(raw) > 72 or any(c in password for c in "\x00\r\n"):
        raise ValueError("Используйте сгенерированный пароль от 16 символов, до 72 байт.")
    result = subprocess.run([caddy, "hash-password", "--algorithm", "bcrypt"],
                            input=raw + b"\n", capture_output=True, check=True)
    hashed = result.stdout.strip()
    if not re.fullmatch(rb"\$2[aby]\$\d\d\$[./A-Za-z0-9]{53}", hashed):
        raise ValueError("Caddy не вернул bcrypt-хеш. Файл не изменён.")
    target.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=target.parent, delete=False) as staged:
        try:
            staged.write(b"team " + hashed + b"\n")
            staged.flush()
            os.fsync(staged.fileno())
            os.replace(staged.name, target)
        finally:
            Path(staged.name).unlink(missing_ok=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--file", type=Path, default=Path(__file__).parent / "secrets/users.caddy")
    parser.add_argument("--caddy", default="caddy")
    args = parser.parse_args()
    if not sys.stdin.isatty():
        parser.exit(1, "Запустите команду в интерактивном терминале.\n")
    password = getpass.getpass("Новый общий пароль из менеджера паролей: ")
    if not hmac.compare_digest(password.encode(), getpass.getpass("Повторите пароль: ").encode()):
        parser.exit(1, "Пароли различаются.\n")
    try:
        set_password(args.file, password, args.caddy)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        parser.exit(1, f"Не удалось сохранить хеш: {error}\n")
    print(f"Хеш сохранён в {args.file}")
