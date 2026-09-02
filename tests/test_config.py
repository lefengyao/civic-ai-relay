import dataclasses
import json

import pytest

from config import RESTART_REQUIRED_SETTING_NAMES, SECRET_SETTING_NAMES, Settings, SettingsError


def required_values(tmp_path) -> dict[str, str]:
    return {
        "PUBLIC_API_KEY": "public-secret",
        "ADMIN_API_KEY": "admin-secret",
        "UPSTREAM_BASE_URL": "https://provider.example",
        "UPSTREAM_API_KEY": "upstream-secret",
        "MODEL_WHITELIST": "public-chat,public-reasoner",
        "MODEL_MAP_JSON": '{"public-chat":"provider-chat"}',
        "MODEL_AUTO_SYNC": "true",
        "MODEL_SYNC_INTERVAL": "30",
        "TOKEN_LIMIT_5H": "10000",
        "TOKEN_LIMIT_DAILY": "5000",
        "RPM_LIMIT": "30",
        "GLOBAL_CONCURRENCY_LIMIT": "2",
        "MEMORY_LIMIT_MB": "200",
        "MAX_OUTPUT_TOKENS": "256",
        "MAX_BODY_MB": "3",
        "MAX_STREAM_DURATION": "42",
        "RETENTION_DAYS": "14",
        "DB_PATH": str(tmp_path / "relay.db"),
        "HOST": "127.0.0.1",
        "PORT": "9000",
        "LOG_LEVEL": "debug",
        "DOCS_ENABLED": "true",
        "UPSTREAM_CONNECT_TIMEOUT": "1.5",
        "UPSTREAM_READ_TIMEOUT": "2.5",
        "UPSTREAM_WRITE_TIMEOUT": "3.5",
        "UPSTREAM_POOL_TIMEOUT": "4.5",
        "RELAY_ENCRYPTION_KEY": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
    }


def set_required(monkeypatch, **overrides):
    values = {
        "PUBLIC_API_KEY": "public-secret",
        "ADMIN_API_KEY": "admin-secret",
        "UPSTREAM_BASE_URL": "https://provider.example/v1/",
        "UPSTREAM_API_KEY": "upstream-secret",
        "MODEL_WHITELIST": "public-chat, public-reasoner",
        "TOKEN_LIMIT_5H": "10000",
        "TOKEN_LIMIT_DAILY": "5000",
        "RPM_LIMIT": "30",
        "GLOBAL_CONCURRENCY_LIMIT": "2",
        "MAX_OUTPUT_TOKENS": "256",
        "RELAY_ENCRYPTION_KEY": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
    }
    values.update(overrides)
    for name, value in values.items():
        monkeypatch.setenv(name, value)


def test_settings_normalize_v1_base_url_and_model_map(monkeypatch):
    set_required(monkeypatch, MODEL_WHITELIST="public-chat", MODEL_MAP_JSON='{"public-chat":"provider-chat"}')

    settings = Settings.from_env()

    assert settings.upstream_base_url == "https://provider.example"
    assert settings.model_whitelist == ("public-chat",)
    assert settings.model_map == {"public-chat": "provider-chat"}


def test_settings_parse_all_defaults_and_types(monkeypatch):
    set_required(monkeypatch)

    settings = Settings.from_env()

    assert settings.public_api_key == "public-secret"
    assert settings.admin_api_key == "admin-secret"
    assert settings.upstream_api_key == "upstream-secret"
    assert settings.token_limit_5h == 10000
    assert settings.token_limit_daily == 5000
    assert settings.rpm_limit == 30
    assert settings.global_concurrency_limit == 2
    assert settings.memory_limit_mb == 200
    assert settings.max_output_tokens == 256
    assert settings.model_auto_sync is True
    assert settings.model_sync_interval == 30
    assert settings.max_body_bytes == 8 * 1024 * 1024
    assert settings.max_stream_duration == 1800
    assert settings.retention_days == 7
    assert settings.db_path == "data/relay.db"
    assert settings.host == "0.0.0.0"
    assert settings.port == 8000
    assert settings.log_level == "INFO"
    assert settings.docs_enabled is False
    assert settings.upstream_connect_timeout == 10.0
    assert settings.upstream_read_timeout == 300.0
    assert settings.upstream_write_timeout == 30.0
    assert settings.upstream_pool_timeout == 10.0


def test_settings_parse_optional_overrides(monkeypatch, tmp_path):
    set_required(monkeypatch)
    overrides = {
        "MAX_BODY_MB": "3",
        "MAX_STREAM_DURATION": "42",
        "RETENTION_DAYS": "14",
        "DB_PATH": str(tmp_path / "relay.db"),
        "HOST": "127.0.0.1",
        "PORT": "9000",
        "LOG_LEVEL": "debug",
        "DOCS_ENABLED": "true",
        "UPSTREAM_CONNECT_TIMEOUT": "1.5",
        "UPSTREAM_READ_TIMEOUT": "2.5",
        "UPSTREAM_WRITE_TIMEOUT": "3.5",
        "UPSTREAM_POOL_TIMEOUT": "4.5",
        "MODEL_MAP_JSON": '{"public-chat": "provider-chat"}',
    }
    for name, value in overrides.items():
        monkeypatch.setenv(name, value)

    settings = Settings.from_env()

    assert settings.max_body_bytes == 3 * 1024 * 1024
    assert settings.max_stream_duration == 42
    assert settings.retention_days == 14
    assert settings.db_path == str(tmp_path / "relay.db")
    assert settings.host == "127.0.0.1"
    assert settings.port == 9000
    assert settings.log_level == "debug"
    assert settings.docs_enabled is True
    assert settings.upstream_connect_timeout == 1.5
    assert settings.upstream_read_timeout == 2.5
    assert settings.upstream_write_timeout == 3.5
    assert settings.upstream_pool_timeout == 4.5


def test_settings_is_frozen(monkeypatch):
    set_required(monkeypatch)
    settings = Settings.from_env()

    assert dataclasses.is_dataclass(settings)
    with pytest.raises(dataclasses.FrozenInstanceError):
        settings.port = 9001


def test_settings_reject_missing_required_limit(monkeypatch):
    set_required(monkeypatch)
    monkeypatch.delenv("RPM_LIMIT", raising=False)

    with pytest.raises(SettingsError, match="RPM_LIMIT") as error:
        Settings.from_env()

    assert "30" not in str(error.value)


@pytest.mark.parametrize(
    ("name", "value"),
    [
        ("PUBLIC_API_KEY", ""),
        ("ADMIN_API_KEY", " "),
        ("UPSTREAM_API_KEY", " "),
        ("UPSTREAM_BASE_URL", "not a url"),
        ("UPSTREAM_BASE_URL", "http://[::1"),
        ("UPSTREAM_BASE_URL", "https://example.com:bad"),
        ("MODEL_WHITELIST", " , "),
        ("TOKEN_LIMIT_5H", "0"),
        ("TOKEN_LIMIT_DAILY", "-1"),
        ("RPM_LIMIT", "not-an-int"),
        ("GLOBAL_CONCURRENCY_LIMIT", "0"),
        ("MEMORY_LIMIT_MB", "0"),
        ("MAX_OUTPUT_TOKENS", "0"),
        ("MAX_BODY_MB", "0"),
        ("MAX_STREAM_DURATION", "0"),
        ("RETENTION_DAYS", "0"),
        ("PORT", "70000"),
        ("UPSTREAM_READ_TIMEOUT", "nan"),
    ],
)
def test_settings_reject_invalid_values_without_echoing_them(monkeypatch, name, value):
    set_required(monkeypatch)
    monkeypatch.setenv(name, value)

    with pytest.raises(SettingsError, match=name) as error:
        Settings.from_env()

    assert str(error.value) == f"invalid setting: {name}"


def test_settings_reject_invalid_model_map(monkeypatch):
    set_required(monkeypatch, MODEL_MAP_JSON=json.dumps({"public-chat": 123}))

    with pytest.raises(SettingsError, match="MODEL_MAP_JSON"):
        Settings.from_env()


def test_settings_reject_invalid_boolean(monkeypatch):
    set_required(monkeypatch, DOCS_ENABLED="sometimes")

    with pytest.raises(SettingsError, match="DOCS_ENABLED"):
        Settings.from_env()


def test_settings_from_mapping_parses_complete_environment(tmp_path):
    values = required_values(tmp_path)

    settings = Settings.from_mapping(values)

    assert settings.admin_api_key == "admin-secret"
    assert settings.to_env_mapping() == values


def test_settings_reject_missing_relay_encryption_key(tmp_path):
    values = required_values(tmp_path)
    values.pop("RELAY_ENCRYPTION_KEY")
    with pytest.raises(SettingsError, match="RELAY_ENCRYPTION_KEY"):
        Settings.from_mapping(values)


def test_relay_encryption_key_is_not_returned_by_redacted_settings(tmp_path):
    values = required_values(tmp_path)
    settings = Settings.from_mapping(values)

    assert settings.relay_encryption_key == values["RELAY_ENCRYPTION_KEY"]
    assert "RELAY_ENCRYPTION_KEY" not in settings.redacted()


def test_settings_redacted_hides_all_secret_values(tmp_path):
    settings = Settings.from_mapping(required_values(tmp_path))

    assert settings.redacted() == {
        "PUBLIC_API_KEY": {"is_configured": True},
        "ADMIN_API_KEY": {"is_configured": True},
        "UPSTREAM_BASE_URL": "https://provider.example",
        "UPSTREAM_API_KEY": {"is_configured": True},
        "MODEL_WHITELIST": "public-chat,public-reasoner",
        "MODEL_MAP_JSON": {"public-chat": "provider-chat"},
        "MODEL_AUTO_SYNC": True,
        "MODEL_SYNC_INTERVAL": 30,
        "TOKEN_LIMIT_5H": 10000,
        "TOKEN_LIMIT_DAILY": 5000,
        "RPM_LIMIT": 30,
        "GLOBAL_CONCURRENCY_LIMIT": 2,
        "MEMORY_LIMIT_MB": 200,
        "MAX_OUTPUT_TOKENS": 256,
        "MAX_BODY_MB": 3,
        "MAX_STREAM_DURATION": 42,
        "RETENTION_DAYS": 14,
        "DB_PATH": str(tmp_path / "relay.db"),
        "HOST": "127.0.0.1",
        "PORT": 9000,
        "LOG_LEVEL": "debug",
        "DOCS_ENABLED": True,
        "UPSTREAM_CONNECT_TIMEOUT": 1.5,
        "UPSTREAM_READ_TIMEOUT": 2.5,
        "UPSTREAM_WRITE_TIMEOUT": 3.5,
        "UPSTREAM_POOL_TIMEOUT": 4.5,
    }
    serialized = json.dumps(settings.redacted())
    for secret in ("public-secret", "admin-secret", "upstream-secret"):
        assert secret not in serialized


@pytest.mark.parametrize("key", ("public-secret", "秘密"))
def test_settings_reject_admin_key_matching_public_key_without_echoing_secret(monkeypatch, key):
    set_required(monkeypatch, PUBLIC_API_KEY=key, ADMIN_API_KEY=key)

    with pytest.raises(SettingsError, match="ADMIN_API_KEY") as error:
        Settings.from_env()

    assert str(error.value) == "invalid setting: ADMIN_API_KEY"
    assert key not in str(error.value)


def test_settings_setting_name_groups_are_complete():
    assert SECRET_SETTING_NAMES == frozenset({"PUBLIC_API_KEY", "UPSTREAM_API_KEY", "ADMIN_API_KEY", "RELAY_ENCRYPTION_KEY"})
    assert RESTART_REQUIRED_SETTING_NAMES == frozenset({"HOST", "PORT", "DB_PATH", "DOCS_ENABLED"})
