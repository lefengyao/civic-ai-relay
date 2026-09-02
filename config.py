"""Environment-backed configuration for Civic Relay."""

from __future__ import annotations

import hmac
import json
import math
import os
from dataclasses import dataclass
from typing import Any, Mapping
from urllib.parse import urlparse


class SettingsError(ValueError):
    """Raised when an environment setting is missing or invalid."""


SECRET_SETTING_NAMES = frozenset({"PUBLIC_API_KEY", "UPSTREAM_API_KEY", "ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY"})
RESTART_REQUIRED_SETTING_NAMES = frozenset({"HOST", "PORT", "DB_PATH", "DOCS_ENABLED"})


def _invalid(name: str) -> SettingsError:
    # Keep configuration failures useful without ever echoing secret values.
    return SettingsError(f"invalid setting: {name}")


def required(values: Mapping[str, str], name: str) -> str:
    value = values.get(name)
    if value is None or not value.strip():
        raise _invalid(name)
    return value.strip()


def required_url(values: Mapping[str, str], name: str) -> str:
    value = required(values, name)
    try:
        parsed = urlparse(value)
        # Accessing ``port`` validates malformed and out-of-range ports.
        port = parsed.port
    except ValueError:
        raise _invalid(name) from None
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise _invalid(name)
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise _invalid(name)
    if port is not None and not 1 <= port <= 65535:
        raise _invalid(name)
    return value


def positive_int(values: Mapping[str, str], name: str, default: int | None = None) -> int:
    raw = values.get(name)
    if raw is None:
        if default is None:
            raise _invalid(name)
        return default
    try:
        value = int(raw.strip())
    except (AttributeError, TypeError, ValueError):
        raise _invalid(name) from None
    if value <= 0:
        raise _invalid(name)
    return value


def positive_float(values: Mapping[str, str], name: str, default: float | None = None) -> float:
    raw = values.get(name)
    if raw is None:
        if default is None:
            raise _invalid(name)
        return float(default)
    try:
        value = float(raw.strip())
    except (AttributeError, TypeError, ValueError):
        raise _invalid(name) from None
    if not math.isfinite(value) or value <= 0:
        raise _invalid(name)
    return value


def parse_models(values: Mapping[str, str], name: str) -> tuple[str, ...]:
    raw = values.get(name)
    if raw is None:
        raise _invalid(name)
    models = tuple(part.strip() for part in raw.split(",") if part.strip())
    if not models:
        raise _invalid(name)
    return models


def parse_model_map(values: Mapping[str, str], name: str) -> dict[str, str]:
    raw = values.get(name)
    if raw is None or not raw.strip():
        return {}
    try:
        value: Any = json.loads(raw)
    except (TypeError, ValueError, json.JSONDecodeError):
        raise _invalid(name) from None
    if not isinstance(value, dict):
        raise _invalid(name)
    if any(
        not isinstance(key, str)
        or not key.strip()
        or not isinstance(mapped, str)
        or not mapped.strip()
        for key, mapped in value.items()
    ):
        raise _invalid(name)
    return {key.strip(): mapped.strip() for key, mapped in value.items()}


def parse_bool(values: Mapping[str, str], name: str, default: bool = False) -> bool:
    raw = values.get(name)
    if raw is None:
        return default
    normalized = raw.strip().lower()
    if normalized in {"true", "1", "yes", "on"}:
        return True
    if normalized in {"false", "0", "no", "off"}:
        return False
    raise _invalid(name)


def _optional_text(values: Mapping[str, str], name: str, default: str) -> str:
    raw = values.get(name)
    if raw is None:
        return default
    value = raw.strip()
    if not value:
        raise _invalid(name)
    return value


@dataclass(frozen=True)
class Settings:
    public_api_key: str
    admin_api_key: str
    upstream_base_url: str
    upstream_api_key: str
    model_whitelist: tuple[str, ...]
    model_map: dict[str, str]
    # 控制后台模型同步；关闭后仍可通过管理台手动同步。
    model_auto_sync: bool
    # 以分钟表示后台同步间隔，避免在高频请求路径访问上游模型目录。
    model_sync_interval: int
    token_limit_5h: int
    token_limit_daily: int
    rpm_limit: int
    global_concurrency_limit: int
    max_output_tokens: int
    max_body_bytes: int
    max_stream_duration: int
    retention_days: int
    db_path: str
    host: str
    port: int
    log_level: str
    docs_enabled: bool
    upstream_connect_timeout: float
    upstream_read_timeout: float
    upstream_write_timeout: float
    upstream_pool_timeout: float
    relay_encryption_key: str

    @classmethod
    def from_mapping(cls, values: Mapping[str, str], *, allow_unconfigured: bool = False) -> "Settings":
        public_api_key = values.get("PUBLIC_API_KEY", "").strip() if allow_unconfigured else required(values, "PUBLIC_API_KEY")
        admin_api_key = required(values, "ADMIN_API_KEY")
        if public_api_key and hmac.compare_digest(admin_api_key.encode(), public_api_key.encode()):
            raise _invalid("ADMIN_API_KEY")

        base_url = required_url(values, "UPSTREAM_BASE_URL").rstrip("/") if not allow_unconfigured or values.get("UPSTREAM_BASE_URL", "").strip() else ""
        if base_url.endswith("/v1"):
            base_url = base_url[:-3]

        port = positive_int(values, "PORT", 8000)
        if port > 65535:
            raise _invalid("PORT")

        return cls(
            public_api_key=public_api_key,
            admin_api_key=admin_api_key,
            upstream_base_url=base_url,
            upstream_api_key=values.get("UPSTREAM_API_KEY", "").strip() if allow_unconfigured else required(values, "UPSTREAM_API_KEY"),
            model_whitelist=(parse_models(values, "MODEL_WHITELIST") if values.get("MODEL_WHITELIST", "").strip() else (() if allow_unconfigured else parse_models(values, "MODEL_WHITELIST"))),
            model_map=parse_model_map(values, "MODEL_MAP_JSON"),
            model_auto_sync=parse_bool(values, "MODEL_AUTO_SYNC", True),
            model_sync_interval=positive_int(values, "MODEL_SYNC_INTERVAL", 30),
            token_limit_5h=positive_int(values, "TOKEN_LIMIT_5H"),
            token_limit_daily=positive_int(values, "TOKEN_LIMIT_DAILY"),
            rpm_limit=positive_int(values, "RPM_LIMIT"),
            global_concurrency_limit=positive_int(values, "GLOBAL_CONCURRENCY_LIMIT"),
            max_output_tokens=positive_int(values, "MAX_OUTPUT_TOKENS"),
            max_body_bytes=positive_int(values, "MAX_BODY_MB", 8) * 1024 * 1024,
            max_stream_duration=positive_int(values, "MAX_STREAM_DURATION", 1800),
            retention_days=positive_int(values, "RETENTION_DAYS", 7),
            db_path=_optional_text(values, "DB_PATH", "data/relay.db"),
            host=_optional_text(values, "HOST", "0.0.0.0"),
            port=port,
            log_level=_optional_text(values, "LOG_LEVEL", "INFO"),
            docs_enabled=parse_bool(values, "DOCS_ENABLED", False),
            upstream_connect_timeout=positive_float(values, "UPSTREAM_CONNECT_TIMEOUT", 10),
            upstream_read_timeout=positive_float(values, "UPSTREAM_READ_TIMEOUT", 300),
            upstream_write_timeout=positive_float(values, "UPSTREAM_WRITE_TIMEOUT", 30),
            upstream_pool_timeout=positive_float(values, "UPSTREAM_POOL_TIMEOUT", 10),
            relay_encryption_key=required(values, "RELAY_ENCRYPTION_KEY"),
        )

    @classmethod
    def from_env(cls) -> "Settings":
        return cls.from_mapping(os.environ)

    def to_env_mapping(self) -> dict[str, str]:
        megabyte = 1024 * 1024
        if self.max_body_bytes % megabyte:
            raise _invalid("MAX_BODY_MB")

        return {
            "PUBLIC_API_KEY": self.public_api_key,
            "ADMIN_API_KEY": self.admin_api_key,
            "UPSTREAM_BASE_URL": self.upstream_base_url,
            "UPSTREAM_API_KEY": self.upstream_api_key,
            "MODEL_WHITELIST": ",".join(self.model_whitelist),
            "MODEL_MAP_JSON": json.dumps(self.model_map, separators=(",", ":")),
            "MODEL_AUTO_SYNC": "true" if self.model_auto_sync else "false",
            "MODEL_SYNC_INTERVAL": str(self.model_sync_interval),
            "TOKEN_LIMIT_5H": str(self.token_limit_5h),
            "TOKEN_LIMIT_DAILY": str(self.token_limit_daily),
            "RPM_LIMIT": str(self.rpm_limit),
            "GLOBAL_CONCURRENCY_LIMIT": str(self.global_concurrency_limit),
            "MAX_OUTPUT_TOKENS": str(self.max_output_tokens),
            "MAX_BODY_MB": str(self.max_body_bytes // megabyte),
            "MAX_STREAM_DURATION": str(self.max_stream_duration),
            "RETENTION_DAYS": str(self.retention_days),
            "DB_PATH": self.db_path,
            "HOST": self.host,
            "PORT": str(self.port),
            "LOG_LEVEL": self.log_level,
            "DOCS_ENABLED": "true" if self.docs_enabled else "false",
            "UPSTREAM_CONNECT_TIMEOUT": str(self.upstream_connect_timeout),
            "UPSTREAM_READ_TIMEOUT": str(self.upstream_read_timeout),
            "UPSTREAM_WRITE_TIMEOUT": str(self.upstream_write_timeout),
            "UPSTREAM_POOL_TIMEOUT": str(self.upstream_pool_timeout),
            "RELAY_ENCRYPTION_KEY": self.relay_encryption_key,
        }

    def redacted(self) -> dict[str, object]:
        return {
            "PUBLIC_API_KEY": {"is_configured": bool(self.public_api_key)},
            "ADMIN_API_KEY": {"is_configured": bool(self.admin_api_key)},
            "UPSTREAM_BASE_URL": self.upstream_base_url,
            "UPSTREAM_API_KEY": {"is_configured": bool(self.upstream_api_key)},
            "MODEL_WHITELIST": ",".join(self.model_whitelist),
            "MODEL_MAP_JSON": dict(self.model_map),
            "MODEL_AUTO_SYNC": self.model_auto_sync,
            "MODEL_SYNC_INTERVAL": self.model_sync_interval,
            "TOKEN_LIMIT_5H": self.token_limit_5h,
            "TOKEN_LIMIT_DAILY": self.token_limit_daily,
            "RPM_LIMIT": self.rpm_limit,
            "GLOBAL_CONCURRENCY_LIMIT": self.global_concurrency_limit,
            "MAX_OUTPUT_TOKENS": self.max_output_tokens,
            "MAX_BODY_MB": self.max_body_bytes // (1024 * 1024),
            "MAX_STREAM_DURATION": self.max_stream_duration,
            "RETENTION_DAYS": self.retention_days,
            "DB_PATH": self.db_path,
            "HOST": self.host,
            "PORT": self.port,
            "LOG_LEVEL": self.log_level,
            "DOCS_ENABLED": self.docs_enabled,
            "UPSTREAM_CONNECT_TIMEOUT": self.upstream_connect_timeout,
            "UPSTREAM_READ_TIMEOUT": self.upstream_read_timeout,
            "UPSTREAM_WRITE_TIMEOUT": self.upstream_write_timeout,
            "UPSTREAM_POOL_TIMEOUT": self.upstream_pool_timeout,
        }
