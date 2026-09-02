"""Instance-local concurrency and conservative character token estimates."""

from __future__ import annotations

import asyncio
import math


def estimate_tokens(text: str) -> int:
    """Estimate tokens conservatively without a tokenizer dependency."""
    return max(1, math.ceil(len(text) / 2))


class ConcurrencyGuard:
    """Non-blocking semaphore-like guard for the single worker process."""

    def __init__(self, limit: int):
        if limit < 1:
            raise ValueError("limit must be positive")
        self._limit = limit
        self._active = 0
        self._lock = asyncio.Lock()

    @property
    def active(self) -> int:
        return self._active

    @property
    def limit(self) -> int:
        return self._limit

    async def set_limit(self, limit: int) -> None:
        if limit < 1:
            raise ValueError("limit must be positive")
        async with self._lock:
            self._limit = limit

    async def snapshot(self) -> tuple[int, int]:
        async with self._lock:
            return self._active, self._limit

    async def try_acquire(self) -> bool:
        async with self._lock:
            if self._active >= self._limit:
                return False
            self._active += 1
            return True

    async def release(self) -> None:
        async with self._lock:
            if self._active <= 0:
                raise RuntimeError("concurrency guard released without a lease")
            self._active -= 1


class KeyConcurrencyRegistry:
    """Per-client-key non-blocking concurrency limits for a single worker."""

    def __init__(self):
        self._guards: dict[str, ConcurrencyGuard] = {}
        self._lock = asyncio.Lock()

    async def try_acquire(self, key_id: str | int, limit: int) -> bool:
        key = str(key_id)
        async with self._lock:
            guard = self._guards.get(key)
            if guard is None:
                guard = self._guards[key] = ConcurrencyGuard(limit)
            elif guard.limit != limit:
                await guard.set_limit(limit)
        return await guard.try_acquire()

    async def release(self, key_id: str | int) -> None:
        key = str(key_id)
        async with self._lock:
            guard = self._guards.get(key)
        if guard is None:
            raise RuntimeError("unknown key concurrency lease")
        await guard.release()

    async def snapshot(self, key_id: str | int) -> tuple[int, int]:
        async with self._lock:
            guard = self._guards.get(str(key_id))
        return (0, 0) if guard is None else await guard.snapshot()
