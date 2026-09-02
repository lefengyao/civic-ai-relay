"""Runtime registry for encrypted multi-provider upstream clients."""

from __future__ import annotations

import asyncio
from typing import Any

from tenant_store import ModelRecord, TenantStore
from upstream import UpstreamClient, UpstreamError


class ProviderRegistry:
    def __init__(self, store: TenantStore, encryption_key: str | None = None, http_client: Any | None = None):
        self.store = store
        self._http_client = http_client
        self._clients: dict[int, UpstreamClient] = {}
        self._fingerprints: dict[int, tuple[str, str]] = {}
        self._lock = asyncio.Lock()

    async def client_for_model(self, model_id: int) -> UpstreamClient:
        model = await self.store.get_model(model_id)
        if model is None:
            raise KeyError("model not found")
        provider, api_key = await self.store.provider_credentials(model.provider_id)
        if not provider.enabled:
            raise UpstreamError(None, "provider_disabled", "provider disabled")
        fingerprint = (provider.base_url, api_key)
        async with self._lock:
            existing = self._clients.get(provider.id)
            if existing is None or self._fingerprints.get(provider.id) != fingerprint:
                if existing is not None:
                    await existing.close()
                existing = UpstreamClient(provider.base_url, api_key, client=self._http_client)
                self._clients[provider.id] = existing
                self._fingerprints[provider.id] = fingerprint
            return existing

    async def sync_provider(self, provider_id: int) -> tuple[list[str], list[str] | None]:
        provider, _ = await self.store.provider_credentials(provider_id)
        if not provider.enabled:
            return [], None
        # Use a provider-specific client so synchronization failures are isolated.
        client = await self._client_for_provider(provider_id)
        model_ids = await client.list_models()
        imported = await self.store.sync_provider_models(provider_id, model_ids)
        return list(model_ids), imported

    async def _client_for_provider(self, provider_id: int) -> UpstreamClient:
        providers = await self.store.list_providers()
        provider = next((item for item in providers if item.id == provider_id), None)
        if provider is None:
            raise KeyError("provider not found")
        _, api_key = await self.store.provider_credentials(provider_id)
        fingerprint = (provider.base_url, api_key)
        async with self._lock:
            existing = self._clients.get(provider_id)
            if existing is None or self._fingerprints.get(provider_id) != fingerprint:
                if existing is not None:
                    await existing.close()
                existing = UpstreamClient(provider.base_url, api_key, client=self._http_client)
                self._clients[provider_id] = existing
                self._fingerprints[provider_id] = fingerprint
            return existing

    async def close(self) -> None:
        async with self._lock:
            clients = list(self._clients.values())
            self._clients.clear(); self._fingerprints.clear()
        for client in clients:
            await client.close()

