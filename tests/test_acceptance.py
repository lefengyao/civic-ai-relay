from __future__ import annotations

import asyncio

import httpx
import pytest

from app import create_app
from tests.conftest import FakeUpstream, auth, build_settings, payload


@pytest.mark.asyncio
async def test_models_requires_bearer_and_returns_public_whitelist(app_client):
    client, _ = app_client
    response = await client.get("/v1/models")
    assert response.status_code == 401
    response = await client.get("/v1/models", headers=auth())
    assert response.status_code == 200
    assert response.json()["data"][0]["id"] == "public-chat"


@pytest.mark.asyncio
async def test_non_stream_chat_settles_usage_and_request_id(app_client):
    client, ledger = app_client
    response = await client.post("/v1/chat/completions", json=payload(), headers=auth())
    assert response.status_code == 200
    assert response.headers["x-request-id"]
    row = await ledger.latest()
    assert row["status"] == "completed"
    assert row["charged_tokens"] == 3
    assert row["reserved_tokens"] == 0


@pytest.mark.asyncio
async def test_model_and_output_token_validation(app_client):
    client, _ = app_client
    denied = await client.post("/v1/chat/completions", json=payload(model="other"), headers=auth())
    assert denied.status_code == 400
    too_large = await client.post("/v1/chat/completions", json=payload(max_tokens=65), headers=auth())
    assert too_large.status_code == 400
    conflict = await client.post(
        "/v1/chat/completions", json=payload(max_tokens=10, max_completion_tokens=11), headers=auth()
    )
    assert conflict.status_code == 400


@pytest.mark.asyncio
async def test_model_map_is_applied_only_to_upstream_payload(tmp_path):
    settings = build_settings(tmp_path, model_map={"public-chat": "provider-chat"})
    upstream = FakeUpstream()
    application = create_app(settings, upstream=upstream)
    transport = httpx.ASGITransport(app=application)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        response = await client.post("/v1/chat/completions", json=payload(), headers=auth())
    assert response.status_code == 200
    assert upstream.last_payload["model"] == "provider-chat"


@pytest.mark.asyncio
async def test_stream_without_usage_is_charged_and_releases_slot(tmp_path):
    settings = build_settings(tmp_path, global_concurrency_limit=1)
    upstream = FakeUpstream(stream_chunks=[b'data: {"choices":[{"delta":{"content":"1234567890"}}]}\n\n'])
    application = create_app(settings, upstream=upstream)
    transport = httpx.ASGITransport(app=application)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        response = await client.post("/v1/chat/completions", json=payload(stream=True), headers=auth())
        assert response.status_code == 200
        assert b"data:" in response.content
        row = await application.state.ledger.latest()
        assert row["charged_tokens"] > 0
        assert row["reserved_tokens"] == 0


@pytest.mark.asyncio
async def test_stream_holds_concurrency_slot_until_completion(tmp_path):
    settings = build_settings(tmp_path, global_concurrency_limit=1)
    chunks = [b'data: {"choices":[{"delta":{"content":"a"}}]}\n\n'] * 3
    upstream = FakeUpstream(stream_chunks=chunks, delay=0.05)
    application = create_app(settings, upstream=upstream)
    transport = httpx.ASGITransport(app=application)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        first = asyncio.create_task(client.post("/v1/chat/completions", json=payload(stream=True), headers=auth()))
        await asyncio.sleep(0.06)
        second = await client.post("/v1/chat/completions", json=payload(), headers=auth())
        assert second.status_code == 429
        assert (await first).status_code == 200
        third = await client.post("/v1/chat/completions", json=payload(), headers=auth())
        assert third.status_code == 200


@pytest.mark.asyncio
async def test_healthz_is_public_and_docs_are_disabled(app_client):
    client, _ = app_client
    assert (await client.get("/healthz")).json() == {"status": "ok"}
    assert (await client.get("/docs")).status_code == 404


@pytest.mark.asyncio
async def test_each_client_sees_authorized_models_and_routes_by_public_alias(tmp_path, monkeypatch):
    settings = build_settings(tmp_path)
    legacy_upstream = FakeUpstream()
    routed_upstream = FakeUpstream()
    application = create_app(settings, upstream=legacy_upstream)
    store = application.state.tenant_store
    provider = await store.create_provider("provider-a", "https://a.example", "secret-a")
    model = await store.create_model("provider-a/chat", provider.id, "chat-internal", 0, 0, True)
    group = await store.create_model_group("基础组")
    await store.set_group_models(group.id, [model.id])
    key = await store.create_client_key("alice", 1, None, None)
    await store.set_key_groups(key.id, [group.id])

    async def client_for_model(model_id: int):
        assert model_id == model.id
        return routed_upstream

    monkeypatch.setattr(application.state.provider_registry, "client_for_model", client_for_model)
    transport = httpx.ASGITransport(app=application)
    headers = {"Authorization": f"Bearer {key.token}"}
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        models = await client.get("/v1/models", headers=headers)
        response = await client.post("/v1/chat/completions", headers=headers, json=payload(model="provider-a/chat"))

    assert [item["id"] for item in models.json()["data"]] == ["provider-a/chat"]
    assert response.status_code == 200
    assert routed_upstream.last_payload["model"] == "chat-internal"
