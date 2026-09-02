from __future__ import annotations

import asyncio

import pytest

from app import create_app
import app as app_module
from config_store import ConfigStore, parse_env_text, serialize_env_mapping
from tests.conftest import FakeUpstream, admin_headers, auth, build_settings, payload


@pytest.mark.asyncio
async def test_admin_endpoints_require_independent_admin_key(admin_client):
    client, _, _ = admin_client
    for path in ("/admin/api/overview", "/admin/api/requests", "/admin/api/config"):
        assert (await client.get(path)).status_code == 401
        assert (await client.get(path, headers=admin_headers("public-test"))).status_code == 401

    response = await client.get("/admin/api/overview", headers=admin_headers())
    assert response.status_code == 200
    assert response.json()["concurrency"] == {"active": 0, "limit": 2}
    assert response.json()["rpm"] == {"used": 0, "limit": 30}


@pytest.mark.asyncio
async def test_admin_configuration_remains_available_while_model_requests_are_memory_limited(admin_client, monkeypatch):
    client, _, _ = admin_client
    monkeypatch.setattr(app_module, "process_memory_mb", lambda: 201.0)

    response = await client.get("/admin/api/config", headers=admin_headers())

    assert response.status_code == 200
    assert response.json()["settings"]["MEMORY_LIMIT_MB"] == 200


@pytest.mark.asyncio
async def test_admin_config_and_request_history_are_redacted(admin_client):
    client, _, _ = admin_client
    assert (await client.post("/v1/chat/completions", json=payload(), headers=auth())).status_code == 200

    config = await client.get("/admin/api/config", headers=admin_headers())
    assert config.status_code == 200
    assert "upstream-secret" not in config.text
    assert "admin-test" not in config.text
    assert config.json()["settings"]["UPSTREAM_API_KEY"] == {"is_configured": True}

    records = await client.get("/admin/api/requests?limit=50", headers=admin_headers())
    assert records.status_code == 200
    assert records.json()["data"][0]["model"] == "public-chat"
    assert "hello" not in records.text
    assert "content" not in records.json()["data"][0]


@pytest.mark.asyncio
async def test_config_save_hot_reloads_public_key_model_and_restart_marker(admin_client):
    client, application, env_path = admin_client
    response = await client.put(
        "/admin/api/config",
        headers=admin_headers(),
        json={"settings": {
            "PUBLIC_API_KEY": "new-public",
            "MODEL_WHITELIST": "new-chat",
            "MODEL_MAP_JSON": '{"new-chat":"provider-new"}',
            "MEMORY_LIMIT_MB": "256",
        }},
    )
    assert response.status_code == 200
    assert response.json()["pending_restart_fields"] == []
    assert "new-public" in env_path.read_text(encoding="utf-8")
    assert (await client.get("/v1/models", headers=auth())).status_code == 401
    models = await client.get("/v1/models", headers={"Authorization": "Bearer new-public"})
    assert models.status_code == 200
    assert models.json()["data"][0]["id"] == "new-chat"
    assert application.state.runtime.settings.memory_limit_mb == 256

    deferred = await client.put(
        "/admin/api/config",
        headers=admin_headers(),
        json={"settings": {"PORT": "9001"}},
    )
    assert deferred.status_code == 200
    assert deferred.json()["pending_restart_fields"] == ["PORT"]


@pytest.mark.asyncio
async def test_admin_key_rotation_invalidates_old_key(admin_client):
    client, _, _ = admin_client
    response = await client.put(
        "/admin/api/config",
        headers=admin_headers(),
        json={"settings": {"ADMIN_API_KEY": "new-admin"}},
    )
    assert response.status_code == 200
    assert (await client.get("/admin/api/overview", headers=admin_headers())).status_code == 401
    assert (await client.get("/admin/api/overview", headers=admin_headers("new-admin"))).status_code == 200


@pytest.mark.asyncio
async def test_admin_sync_models_fetches_persists_and_hot_reloads(admin_client):
    client, application, env_path = admin_client
    application.state.runtime._upstreams[0].model_ids = ("provider-chat", "provider-reasoner")

    response = await client.post("/admin/api/models/sync", headers=admin_headers())

    assert response.status_code == 200
    assert response.json()["models"] == ["provider-chat", "provider-reasoner"]
    saved = parse_env_text(env_path.read_text(encoding="utf-8"))
    assert saved["MODEL_WHITELIST"] == "provider-chat,provider-reasoner"
    models = await client.get("/v1/models", headers=auth())
    assert [model["id"] for model in models.json()["data"]] == ["provider-chat", "provider-reasoner"]


@pytest.mark.asyncio
async def test_startup_sync_replaces_the_saved_model_allowlist(tmp_path):
    settings = build_settings(tmp_path)
    env_path = tmp_path / "relay.env"
    env_path.write_text(serialize_env_mapping(settings.to_env_mapping()), encoding="utf-8")
    application = create_app(
        settings,
        upstream=FakeUpstream(model_ids=("provider-chat",)),
        config_store=ConfigStore(env_path),
    )

    async with application.router.lifespan_context(application):
        for _ in range(20):
            if application.state.runtime.settings.model_whitelist == ("provider-chat",):
                break
            await asyncio.sleep(0.01)

    assert application.state.runtime.settings.model_whitelist == ("provider-chat",)


@pytest.mark.asyncio
async def test_admin_shell_contains_no_configuration_or_persistent_key_storage(admin_client):
    client, _, _ = admin_client

    response = await client.get("/admin")

    assert response.status_code == 200
    assert "Civic Relay" in response.text
    assert "upstream-secret" not in response.text
    assert "admin-test" not in response.text
    assert "localStorage" not in response.text
    assert "sessionStorage" not in response.text
    assert "/admin/api/overview" in response.text
    assert "公共 API 密钥" in response.text
    assert "自动同步模型" in response.text
    assert "供应商" in response.text
    assert "模型组" in response.text
    assert "客户端 Key" in response.text
    assert "一次性 Token 总额度" in response.text


@pytest.mark.asyncio
async def test_admin_overview_degrades_when_the_ledger_snapshot_fails(admin_client, monkeypatch):
    client, application, _ = admin_client

    async def unavailable_snapshot(*_args, **_kwargs):
        raise OSError("ledger unavailable")

    monkeypatch.setattr(application.state.ledger, "monitoring_snapshot", unavailable_snapshot)

    response = await client.get("/admin/api/overview", headers=admin_headers())

    assert response.status_code == 200
    assert response.json()["ledger_status"] == "error"
    assert response.json()["recent"] == []


@pytest.mark.asyncio
async def test_admin_can_create_tenant_resources_and_redacts_key(admin_client):
    client, _, _ = admin_client
    provider = await client.post("/admin/api/providers", headers=admin_headers(), json={"name": "供应商 A", "base_url": "https://a.example", "api_key": "provider-secret"})
    assert provider.status_code == 201
    provider_id = provider.json()["data"]["id"]
    model = await client.post("/admin/api/models", headers=admin_headers(), json={"public_name": "a/chat", "provider_id": provider_id, "upstream_name": "chat-internal", "input_price_yuan_per_million": 0, "output_price_yuan_per_million": 0, "enabled": True})
    assert model.status_code == 201
    group = await client.post("/admin/api/groups", headers=admin_headers(), json={"name": "基础组", "model_ids": [model.json()["data"]["id"]]})
    assert group.status_code == 201
    created = await client.post("/admin/api/keys", headers=admin_headers(), json={"name": "Alice", "max_concurrency": 2, "token_limit_total": 1000, "money_limit_yuan_total": 3, "group_ids": [group.json()["data"]["id"]]})
    assert created.status_code == 201
    assert created.json()["token"].startswith("crk_")
    listed = await client.get("/admin/api/keys", headers=admin_headers())
    assert all(row["token"] is None for row in listed.json()["data"])
