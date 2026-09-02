from __future__ import annotations

import pytest
from cryptography.fernet import Fernet

from provider_registry import ProviderRegistry
from tenant_store import TenantStore


@pytest.mark.asyncio
async def test_provider_api_key_is_encrypted_and_model_routes_to_provider(tmp_path):
    store = TenantStore(tmp_path / "relay.db", Fernet.generate_key().decode())
    try:
        provider = await store.create_provider("供应商 A", "https://a.example", "secret-a")
        model = await store.create_model("a/chat", provider.id, "chat-a", 0, 0, True)
        registry = ProviderRegistry(store)

        client = await registry.client_for_model(model.id)
        raw = await store.raw_provider_row(provider.id)

        assert client.api_key == "secret-a"
        assert "secret-a" not in raw["encrypted_api_key"]
        assert raw["encrypted_api_key"] != "secret-a"
    finally:
        await registry.close()
        await store.close()


@pytest.mark.asyncio
async def test_provider_sync_imports_unpriced_disabled_models(tmp_path):
    store = TenantStore(tmp_path / "relay.db", Fernet.generate_key().decode())
    try:
        provider = await store.create_provider("provider-a", "https://a.example", "secret-a")
        imported = await store.sync_provider_models(provider.id, ["chat", "reasoner"])
        models = await store.list_models()

        assert imported == ["provider-a/chat", "provider-a/reasoner"]
        assert all(not model.enabled and not model.priced for model in models)
    finally:
        await store.close()
