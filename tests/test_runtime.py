from __future__ import annotations

import pytest

from limiter import ConcurrencyGuard
from runtime import RuntimeConfig
from tests.conftest import build_settings


class RecordingUpstream:
    def __init__(self, name: str):
        self.name = name
        self.closed = False

    async def close(self):
        self.closed = True


@pytest.mark.asyncio
async def test_reload_defers_old_upstream_close_until_lease_finishes(tmp_path):
    first = RecordingUpstream("first")
    second = RecordingUpstream("second")
    guard = ConcurrencyGuard(2)
    runtime = RuntimeConfig(build_settings(tmp_path), first, guard, factory=lambda settings: second)

    async with runtime.lease() as leased:
        result = await runtime.reload(build_settings(tmp_path, upstream_api_key="rotated-secret"))

        assert leased.upstream is first
        assert result.pending_restart_fields == ()
        assert first.closed is False

    assert first.closed is True
    async with runtime.lease() as current:
        assert current.upstream is second
        assert current.settings.upstream_api_key == "rotated-secret"


@pytest.mark.asyncio
async def test_static_settings_are_persisted_but_not_applied_until_restart(tmp_path):
    first = RecordingUpstream("first")
    runtime = RuntimeConfig(build_settings(tmp_path, port=8000), first, ConcurrencyGuard(2))

    result = await runtime.reload(build_settings(tmp_path, port=9001))

    assert result.pending_restart_fields == ("PORT",)
    assert runtime.settings.port == 8000
    assert runtime.managed_settings.port == 9001


@pytest.mark.asyncio
async def test_reload_updates_shared_concurrency_limit(tmp_path):
    guard = ConcurrencyGuard(3)
    runtime = RuntimeConfig(build_settings(tmp_path, global_concurrency_limit=3), RecordingUpstream("first"), guard)

    await runtime.reload(build_settings(tmp_path, global_concurrency_limit=1))

    assert await guard.snapshot() == (0, 1)
