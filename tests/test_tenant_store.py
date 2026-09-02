from __future__ import annotations

import pytest
from cryptography.fernet import Fernet

from tenant_store import TenantStore


@pytest.fixture
async def tenant_store(tmp_path):
    store = TenantStore(tmp_path / "relay.db", Fernet.generate_key().decode())
    try:
        yield store
    finally:
        await store.close()


@pytest.mark.asyncio
async def test_create_key_returns_secret_once_and_list_is_redacted(tenant_store):
    created = await tenant_store.create_client_key("alice", 2, 1000, 3_000_000)

    assert created.token.startswith("crk_")
    rows = await tenant_store.list_client_keys()
    assert rows[0]["name"] == "alice"
    assert rows[0]["token"] is None
    assert created.token not in str(rows)


@pytest.mark.asyncio
async def test_key_can_join_multiple_groups_and_models_are_filtered(tenant_store):
    provider = await tenant_store.create_provider("provider", "https://provider.example", "provider-secret")
    basic = await tenant_store.create_model_group("基础组")
    advanced = await tenant_store.create_model_group("高级组")
    basic_model = await tenant_store.create_model("basic", provider.id, "basic", 0, 0, True)
    advanced_model = await tenant_store.create_model("advanced", provider.id, "advanced", 0, 0, True)
    await tenant_store.set_group_models(basic.id, [basic_model.id])
    await tenant_store.set_group_models(advanced.id, [advanced_model.id])
    key = await tenant_store.create_client_key("alice", 2, None, None)
    await tenant_store.set_key_groups(key.id, [basic.id, advanced.id])

    models = await tenant_store.authorized_models(key.token)

    assert {model.public_name for model in models} == {"basic", "advanced"}


@pytest.mark.asyncio
async def test_unpriced_model_cannot_be_added_to_group(tenant_store):
    provider = await tenant_store.create_provider("provider", "https://provider.example", "provider-secret")
    group = await tenant_store.create_model_group("基础组")
    model = await tenant_store.create_model("new", provider.id, "new", None, None, False)

    with pytest.raises(ValueError, match="model must be priced and enabled"):
        await tenant_store.set_group_models(group.id, [model.id])


@pytest.mark.asyncio
async def test_key_reservations_do_not_enter_global_request_ledger(tenant_store):
    provider = await tenant_store.create_provider("provider", "https://provider.example", "provider-secret")
    group = await tenant_store.create_model_group("基础组")
    model = await tenant_store.create_model("paid", provider.id, "paid", 0, 0, True)
    await tenant_store.set_group_models(group.id, [model.id])
    key = await tenant_store.create_client_key("alice", 1, 10, 100)
    await tenant_store.set_key_groups(key.id, [group.id])

    reservation = await tenant_store.reserve_for_key(key.id, model.id, 10, 0, 10, 0)
    await tenant_store.settle_for_key(reservation.id, 10, 0, 10, 0)

    current = await tenant_store.get_client_key(key.id)
    assert current.enabled is False
    assert current.disabled_reason == "quota_exhausted"
