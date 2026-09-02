import pytest

from limiter import ConcurrencyGuard, KeyConcurrencyRegistry, estimate_tokens


def test_character_estimate_is_never_zero():
    assert estimate_tokens("") == 1
    assert estimate_tokens("abc") == 2


@pytest.mark.asyncio
async def test_concurrency_guard_rejects_without_waiting():
    guard = ConcurrencyGuard(limit=1)
    assert await guard.try_acquire() is True
    assert await guard.try_acquire() is False
    await guard.release()
    assert await guard.try_acquire() is True


@pytest.mark.asyncio
async def test_lowered_limit_keeps_existing_leases_but_rejects_new_work():
    guard = ConcurrencyGuard(limit=2)
    assert await guard.try_acquire() is True
    assert await guard.try_acquire() is True

    await guard.set_limit(1)

    assert guard.active == 2
    assert guard.limit == 1
    assert await guard.try_acquire() is False
    await guard.release()
    assert await guard.try_acquire() is False
    await guard.release()
    assert await guard.try_acquire() is True


@pytest.mark.asyncio
async def test_snapshot_returns_active_and_current_limit():
    guard = ConcurrencyGuard(limit=3)
    assert await guard.try_acquire() is True

    assert await guard.snapshot() == (1, 3)


@pytest.mark.asyncio
async def test_key_concurrency_guard_is_scoped_per_key():
    guard = KeyConcurrencyRegistry()
    assert await guard.try_acquire("a", 1)
    assert not await guard.try_acquire("a", 1)
    assert await guard.try_acquire("b", 1)
    await guard.release("a")
    assert await guard.try_acquire("a", 1)
