from __future__ import annotations

import os
from pathlib import Path

import httpx
import pytest

from config import Settings, SettingsError
from config_store import ConfigStore, ensure_managed_config, managed_config_path, parse_env_text


def required_body() -> str:
    return "\n".join(
        (
            "UPSTREAM_BASE_URL=https://provider.example",
            "MODEL_WHITELIST=public-chat",
            "MODEL_MAP_JSON={}",
            "TOKEN_LIMIT_5H=10000",
            "TOKEN_LIMIT_DAILY=5000",
            "RPM_LIMIT=30",
            "GLOBAL_CONCURRENCY_LIMIT=2",
            "MAX_OUTPUT_TOKENS=256",
            "MAX_BODY_MB=8",
            "MAX_STREAM_DURATION=1800",
            "RETENTION_DAYS=7",
            "DB_PATH=data/relay.db",
            "HOST=127.0.0.1",
            "PORT=8000",
            "LOG_LEVEL=INFO",
            "DOCS_ENABLED=false",
            "UPSTREAM_CONNECT_TIMEOUT=10",
            "UPSTREAM_READ_TIMEOUT=300",
            "UPSTREAM_WRITE_TIMEOUT=30",
            "UPSTREAM_POOL_TIMEOUT=10",
            "RELAY_ENCRYPTION_KEY=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
            "",
        )
    )


def write_valid_env(tmp_path):
    path = tmp_path / ".env"
    path.write_text(
        "# keep this variable\nEXTRA_VALUE=kept\n"
        "PUBLIC_API_KEY=public\nADMIN_API_KEY=admin\nUPSTREAM_API_KEY=upstream\n"
        + required_body(),
        encoding="utf-8",
    )
    return path


def test_default_managed_config_path_is_outside_project(monkeypatch):
    monkeypatch.delenv("CIVIC_RELAY_CONFIG_FILE", raising=False)

    path = managed_config_path()

    assert "civicrelay" in str(path).lower()
    assert path.name == "relay.env"


def test_store_creates_parent_directory_before_atomic_write(tmp_path):
    source = write_valid_env(tmp_path)
    target = tmp_path / "nested" / "relay.env"
    store = ConfigStore(target)

    values = parse_env_text(source.read_text(encoding="utf-8"))
    store.write(Settings.from_mapping(values))

    assert target.exists()
    assert target.parent.exists()


def test_store_writes_candidate_and_preserves_blank_secret(tmp_path):
    path = write_valid_env(tmp_path)
    store = ConfigStore(path)

    saved = store.build_candidate({"RPM_LIMIT": "45", "UPSTREAM_API_KEY": ""})
    store.write(saved)

    values = store.read_mapping()
    assert values["RPM_LIMIT"] == "45"
    assert values["UPSTREAM_API_KEY"] == "upstream"
    assert values["EXTRA_VALUE"] == "kept"


def test_store_rejects_invalid_candidate_without_changing_file(tmp_path):
    path = write_valid_env(tmp_path)
    before = path.read_text(encoding="utf-8")
    store = ConfigStore(path)

    with pytest.raises(SettingsError, match="RPM_LIMIT"):
        store.build_candidate({"RPM_LIMIT": "0"})

    assert path.read_text(encoding="utf-8") == before


def test_store_can_merge_missing_new_setting_from_runtime_baseline(tmp_path):
    path = write_valid_env(tmp_path)
    baseline = ConfigStore(path).build_candidate({})
    values = parse_env_text(path.read_text(encoding="utf-8"))
    del values["ADMIN_API_KEY"]
    path.write_text("\n".join(f"{name}={value}" for name, value in values.items()) + "\n", encoding="utf-8")
    store = ConfigStore(path)

    candidate = store.build_candidate({"RPM_LIMIT": "45"}, baseline)
    store.write(candidate)

    assert store.read_mapping()["ADMIN_API_KEY"] == "admin"


def test_store_keeps_original_file_if_atomic_replace_fails(tmp_path, monkeypatch):
    path = write_valid_env(tmp_path)
    store = ConfigStore(path)
    candidate = store.build_candidate({"RPM_LIMIT": "45"})
    before = path.read_text(encoding="utf-8")

    def fail_replace(source, destination):
        raise OSError("replace denied")

    monkeypatch.setattr(os, "replace", fail_replace)

    with pytest.raises(OSError, match="replace denied"):
        store.write(candidate)

    assert path.read_text(encoding="utf-8") == before
    assert list(tmp_path.glob("..env.*")) == []


def test_parse_env_text_supports_comments_export_and_quoted_values():
    values = parse_env_text('''\n# comment\nexport A=plain\nB="a value # with quote"\nC='single quoted'\n''')

    assert values == {"A": "plain", "B": "a value # with quote", "C": "single quoted"}


def test_initial_managed_config_generates_persistent_admin_key_without_provider(tmp_path):
    target = tmp_path / "CivicRelay" / "relay.env"

    first, created = ensure_managed_config(target)
    second, created_again = ensure_managed_config(target)

    assert created is True
    assert created_again is False
    assert first.admin_api_key.startswith("adm_")
    assert first.admin_api_key == second.admin_api_key
    assert first.relay_encryption_key == second.relay_encryption_key
    assert first.public_api_key == ""
    assert first.upstream_base_url == ""
    assert first.upstream_api_key == ""
    assert first.model_whitelist == ()
    assert first.memory_limit_mb == 200
    assert (target.parent / "bootstrap-admin-key.txt").read_text(encoding="utf-8").strip() == first.admin_api_key


def test_managed_config_path_uses_explicit_value_or_project_default(monkeypatch, tmp_path):
    monkeypatch.delenv("CIVIC_RELAY_CONFIG_FILE", raising=False)
    assert managed_config_path().name == "relay.env"
    assert "civicrelay" in str(managed_config_path()).lower()
    target = tmp_path / "managed.env"
    monkeypatch.setenv("CIVIC_RELAY_CONFIG_FILE", str(target))
    assert managed_config_path() == target


@pytest.mark.asyncio
async def test_create_app_initializes_an_empty_external_config_and_admin_console(tmp_path, monkeypatch):
    """A clean install must expose administration before any provider exists."""
    from app import create_app

    for name in (
        "PUBLIC_API_KEY", "ADMIN_API_KEY", "UPSTREAM_BASE_URL", "UPSTREAM_API_KEY",
        "MODEL_WHITELIST", "RELAY_ENCRYPTION_KEY", "CIVIC_RELAY_CONFIG_FILE",
    ):
        monkeypatch.delenv(name, raising=False)
    config_path = tmp_path / "external-config" / "relay.env"
    monkeypatch.setenv("CIVIC_RELAY_CONFIG_FILE", str(config_path))
    monkeypatch.chdir(tmp_path)

    application = create_app()
    bootstrap_key = (config_path.parent / "bootstrap-admin-key.txt").read_text(encoding="utf-8").strip()
    transport = httpx.ASGITransport(app=application)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        health = await client.get("/healthz")
        configuration = await client.get("/admin/api/config", headers={"X-Admin-Key": bootstrap_key})

    assert health.json() == {"status": "ok"}
    assert configuration.status_code == 200
    assert configuration.json()["settings"]["UPSTREAM_API_KEY"] == {"is_configured": False}
    assert configuration.json()["settings"]["MODEL_WHITELIST"] == ""
