"""Atomic runtime configuration generations for Civic Relay."""

from __future__ import annotations

import asyncio
import logging
from contextlib import asynccontextmanager
from dataclasses import dataclass, replace
from datetime import datetime, timezone
from typing import Any, AsyncIterator, Callable

from config import RESTART_REQUIRED_SETTING_NAMES, Settings
from limiter import ConcurrencyGuard
from upstream import build_upstream


UTC = timezone.utc
_STATIC_ATTRIBUTES = {
    "HOST": "host",
    "PORT": "port",
    "DB_PATH": "db_path",
    "DOCS_ENABLED": "docs_enabled",
}


@dataclass(frozen=True)
class RuntimeLease:
    generation: int
    settings: Settings
    upstream: Any


@dataclass(frozen=True)
class ReloadResult:
    applied_at_utc: str
    pending_restart_fields: tuple[str, ...]


@dataclass(frozen=True)
class PreparedReload:
    generation: int
    managed_settings: Settings
    effective_settings: Settings
    upstream: Any
    replaces_upstream: bool
    pending_restart_fields: tuple[str, ...]


class RuntimeConfig:
    """Lease immutable settings/client generations across configuration swaps."""

    def __init__(
        self,
        settings: Settings,
        upstream: Any,
        guard: ConcurrencyGuard,
        factory: Callable[[Settings], Any] = build_upstream,
    ):
        self._settings = settings
        self._managed_settings = settings
        self._upstreams: dict[int, Any] = {0: upstream}
        self._leases: dict[int, int] = {0: 0}
        self._retired: set[int] = set()
        self._generation = 0
        self._guard = guard
        self._factory = factory
        self._lock = asyncio.Lock()

    @property
    def settings(self) -> Settings:
        return self._settings

    @property
    def managed_settings(self) -> Settings:
        return self._managed_settings

    async def acquire(self) -> RuntimeLease:
        async with self._lock:
            generation = self._generation
            self._leases[generation] += 1
            return RuntimeLease(generation, self._settings, self._upstreams[generation])

    async def release(self, lease: RuntimeLease) -> None:
        to_close = None
        async with self._lock:
            current = self._leases.get(lease.generation)
            if current is None or current <= 0:
                raise RuntimeError("runtime lease released without acquisition")
            self._leases[lease.generation] = current - 1
            to_close = self._retire_if_unused(lease.generation)
        if to_close is not None:
            await to_close.close()

    @asynccontextmanager
    async def lease(self) -> AsyncIterator[RuntimeLease]:
        acquired = await self.acquire()
        try:
            yield acquired
        finally:
            await self.release(acquired)

    def _effective_settings(self, candidate: Settings) -> tuple[Settings, tuple[str, ...]]:
        pending = tuple(
            name
            for name in sorted(RESTART_REQUIRED_SETTING_NAMES)
            if getattr(candidate, _STATIC_ATTRIBUTES[name]) != getattr(self._settings, _STATIC_ATTRIBUTES[name])
        )
        values = {attribute: getattr(self._settings, attribute) for name, attribute in _STATIC_ATTRIBUTES.items() if name in pending}
        return replace(candidate, **values), pending

    @staticmethod
    def _upstream_fingerprint(settings: Settings) -> tuple[object, ...]:
        return (
            settings.upstream_base_url,
            settings.upstream_api_key,
            settings.upstream_connect_timeout,
            settings.upstream_read_timeout,
            settings.upstream_write_timeout,
            settings.upstream_pool_timeout,
        )

    async def prepare_reload(self, candidate: Settings) -> PreparedReload:
        async with self._lock:
            generation = self._generation
            effective, pending = self._effective_settings(candidate)
            current = self._settings
            upstream = self._upstreams[generation]
        replaces = self._upstream_fingerprint(effective) != self._upstream_fingerprint(current)
        if replaces:
            upstream = self._factory(effective)
        return PreparedReload(generation, candidate, effective, upstream, replaces, pending)

    async def discard_prepared_reload(self, prepared: PreparedReload) -> None:
        if prepared.replaces_upstream:
            await prepared.upstream.close()

    async def commit_prepared_reload(self, prepared: PreparedReload) -> ReloadResult:
        to_close = None
        async with self._lock:
            if prepared.generation != self._generation:
                raise RuntimeError("stale runtime reload")
            old_generation = self._generation
            self._generation += 1
            self._settings = prepared.effective_settings
            self._managed_settings = prepared.managed_settings
            self._upstreams[self._generation] = prepared.upstream
            self._leases[self._generation] = 0
            self._retired.add(old_generation)
            to_close = self._retire_if_unused(old_generation)
        await self._guard.set_limit(prepared.effective_settings.global_concurrency_limit)
        logging.getLogger("civic-relay").setLevel(prepared.effective_settings.log_level)
        if to_close is not None:
            await to_close.close()
        return ReloadResult(
            applied_at_utc=datetime.now(UTC).isoformat(timespec="seconds").replace("+00:00", "Z"),
            pending_restart_fields=prepared.pending_restart_fields,
        )

    async def reload(self, candidate: Settings) -> ReloadResult:
        prepared = await self.prepare_reload(candidate)
        try:
            return await self.commit_prepared_reload(prepared)
        except Exception:
            await self.discard_prepared_reload(prepared)
            raise

    def _retire_if_unused(self, generation: int) -> Any | None:
        if generation not in self._retired or self._leases[generation] != 0:
            return None
        upstream = self._upstreams[generation]
        if any(other != generation and client is upstream for other, client in self._upstreams.items()):
            self._retired.remove(generation)
            del self._leases[generation]
            del self._upstreams[generation]
            return None
        self._retired.remove(generation)
        del self._leases[generation]
        del self._upstreams[generation]
        return upstream

    async def close(self) -> None:
        async with self._lock:
            upstreams = {id(client): client for client in self._upstreams.values()}
            self._upstreams.clear()
            self._leases.clear()
            self._retired.clear()
        for client in upstreams.values():
            await client.close()
