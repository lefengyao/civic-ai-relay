"""Managed environment-file parsing and atomic persistence."""

from __future__ import annotations

import ast
import json
import os
import re
import tempfile
from collections.abc import Mapping
from pathlib import Path

from config import SECRET_SETTING_NAMES, Settings


_ENVIRONMENT_KEY = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def managed_config_path() -> Path:
    """Return the immutable deployment-selected configuration path."""
    configured = os.getenv("CIVIC_RELAY_CONFIG_FILE")
    if configured:
        return Path(configured)
    if os.name == "nt":
        # 生产密钥默认存放在项目目录之外，避免源码目录被打包或共享时泄露。
        return Path(os.environ.get("PROGRAMDATA", r"C:\ProgramData")) / "CivicRelay" / "relay.env"
    return Path("/etc/civic-relay/relay.env")


def generate_initial_settings() -> Settings:
    """Create a fresh install configuration without provider credentials."""
    import secrets
    from cryptography.fernet import Fernet

    values = {
        "PUBLIC_API_KEY": "",
        "ADMIN_API_KEY": "adm_" + secrets.token_urlsafe(32),
        "UPSTREAM_BASE_URL": "",
        "UPSTREAM_API_KEY": "",
        "RELAY_ENCRYPTION_KEY": Fernet.generate_key().decode("ascii"),
        "MODEL_WHITELIST": "",
        "MODEL_MAP_JSON": "{}",
        "MODEL_AUTO_SYNC": "false",
        "MODEL_SYNC_INTERVAL": "30",
        "TOKEN_LIMIT_5H": "100000",
        "TOKEN_LIMIT_DAILY": "20000",
        "RPM_LIMIT": "30",
        "GLOBAL_CONCURRENCY_LIMIT": "8",
        "MAX_OUTPUT_TOKENS": "4096",
        "MAX_BODY_MB": "8",
        "MAX_STREAM_DURATION": "1800",
        "RETENTION_DAYS": "7",
        "DB_PATH": "data/relay.db",
        "HOST": "0.0.0.0",
        "PORT": "8000",
        "LOG_LEVEL": "INFO",
        "DOCS_ENABLED": "false",
        "UPSTREAM_CONNECT_TIMEOUT": "10",
        "UPSTREAM_READ_TIMEOUT": "300",
        "UPSTREAM_WRITE_TIMEOUT": "30",
        "UPSTREAM_POOL_TIMEOUT": "10",
    }
    return Settings.from_mapping(values, allow_unconfigured=True)


def ensure_managed_config(path: Path | str | None = None) -> tuple[Settings, bool]:
    """Load external config or create it once for a fresh installation."""
    target = Path(path) if path is not None else managed_config_path()
    store = ConfigStore(target)
    if target.exists():
        values = store.read_mapping()
        allow_unconfigured = not values.get("UPSTREAM_BASE_URL", "").strip() and not values.get("UPSTREAM_API_KEY", "").strip()
        return Settings.from_mapping(values, allow_unconfigured=allow_unconfigured), False
    settings = generate_initial_settings()
    store.write(settings)
    bootstrap = target.parent / "bootstrap-admin-key.txt"
    bootstrap.write_text(settings.admin_api_key + "\n", encoding="utf-8")
    try:
        # Configuration and the one-time bootstrap credential are sensitive.
        # Restrict a newly-created directory where the platform supports POSIX
        # permissions; Windows ACLs are managed by the installer or operator.
        os.chmod(target.parent, 0o700)
        os.chmod(bootstrap, 0o600)
        os.chmod(target, 0o600)
    except OSError:
        pass
    return settings, True


def _parse_value(raw: str, line_number: int) -> str:
    value = raw.strip()
    if not value:
        return ""
    if value[0] == '"':
        try:
            decoded = json.loads(value)
        except (TypeError, ValueError, json.JSONDecodeError):
            raise ValueError(f"invalid configuration value on line {line_number}") from None
        if not isinstance(decoded, str):
            raise ValueError(f"invalid configuration value on line {line_number}")
        return decoded
    if value[0] == "'":
        try:
            decoded = ast.literal_eval(value)
        except (SyntaxError, ValueError):
            raise ValueError(f"invalid configuration value on line {line_number}") from None
        if not isinstance(decoded, str):
            raise ValueError(f"invalid configuration value on line {line_number}")
        return decoded
    return value


def parse_env_text(text: str) -> dict[str, str]:
    """Parse the supported .env subset while keeping unknown valid variables."""
    values: dict[str, str] = {}
    for line_number, line in enumerate(text.splitlines(), start=1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if stripped.startswith("export "):
            stripped = stripped[7:].lstrip()
        if "=" not in stripped:
            raise ValueError(f"invalid configuration line {line_number}")
        name, raw_value = stripped.split("=", 1)
        name = name.strip()
        if not _ENVIRONMENT_KEY.fullmatch(name):
            raise ValueError(f"invalid configuration key on line {line_number}")
        values[name] = _parse_value(raw_value, line_number)
    return values


def _serialize_value(value: str) -> str:
    if not value or value != value.strip() or any(character in value for character in '#\\"\'\n\r\t'):
        return json.dumps(value, ensure_ascii=False)
    return value


def serialize_env_mapping(values: Mapping[str, str]) -> str:
    """Serialize a mapping to a canonical, parseable environment file."""
    lines: list[str] = []
    for name, value in values.items():
        if not _ENVIRONMENT_KEY.fullmatch(name):
            raise ValueError(f"invalid configuration key: {name}")
        if not isinstance(value, str):
            raise ValueError(f"invalid configuration value: {name}")
        lines.append(f"{name}={_serialize_value(value)}")
    return "\n".join(lines) + "\n"


class ConfigStore:
    """Read, validate, and atomically write the managed relay configuration."""

    def __init__(self, path: Path | str | None = None):
        self.path = Path(path) if path is not None else managed_config_path()

    def read_mapping(self) -> dict[str, str]:
        return parse_env_text(self.path.read_text(encoding="utf-8"))

    def build_candidate(self, patch: Mapping[str, str | None], base: Settings | None = None) -> Settings:
        values = base.to_env_mapping() if base is not None else {}
        values.update(self.read_mapping())
        for name, raw_value in patch.items():
            if name in SECRET_SETTING_NAMES and not raw_value:
                continue
            if raw_value is not None:
                values[name] = raw_value
        allow_unconfigured = not values.get("UPSTREAM_BASE_URL", "").strip() and not values.get("UPSTREAM_API_KEY", "").strip()
        return Settings.from_mapping(values, allow_unconfigured=allow_unconfigured)

    def write(self, settings: Settings) -> None:
        values = self.read_mapping() if self.path.exists() else {}
        values.update(settings.to_env_mapping())
        self.path.parent.mkdir(parents=True, exist_ok=True)
        content = serialize_env_mapping(values)
        descriptor, temporary_name = tempfile.mkstemp(
            prefix=f".{self.path.name}.",
            dir=self.path.parent,
            text=True,
        )
        temporary = Path(temporary_name)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8", newline="\n") as handle:
                handle.write(content)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(temporary, self.path)
        except Exception:
            temporary.unlink(missing_ok=True)
            raise
