from __future__ import annotations

import asyncio
import os
from contextlib import asynccontextmanager
from dataclasses import replace
from typing import Any, AsyncIterator

import httpx
import pytest

# The production module intentionally fails fast when required settings are
# absent. Provide harmless collection-time values for importing the test app.
os.environ.setdefault("PUBLIC_API_KEY", "collection-public")
os.environ.setdefault("ADMIN_API_KEY", "collection-admin")
os.environ.setdefault("UPSTREAM_API_KEY", "collection-upstream")
os.environ.setdefault("UPSTREAM_BASE_URL", "https://provider.example")
os.environ.setdefault("MODEL_WHITELIST", "public-chat")
os.environ.setdefault("TOKEN_LIMIT_5H", "10000")
os.environ.setdefault("TOKEN_LIMIT_DAILY", "10000")
os.environ.setdefault("RPM_LIMIT", "30")
os.environ.setdefault("GLOBAL_CONCURRENCY_LIMIT", "2")
os.environ.setdefault("MAX_OUTPUT_TOKENS", "64")
os.environ.setdefault("RELAY_ENCRYPTION_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")

from config import Settings
from config_store import ConfigStore, serialize_env_mapping
from db import Ledger
from limiter import ConcurrencyGuard
from app import create_app


def build_settings(tmp_path, **overrides) -> Settings:
    values = dict(
        public_api_key="public-test",
        admin_api_key="admin-test",
        upstream_base_url="https://provider.example",
        upstream_api_key="upstream-secret",
        model_whitelist=("public-chat",),
        model_map={},
        model_auto_sync=True,
        model_sync_interval=30,
        token_limit_5h=10000,
        token_limit_daily=10000,
        rpm_limit=30,
        global_concurrency_limit=2,
        memory_limit_mb=200,
        max_output_tokens=64,
        max_body_bytes=1024 * 1024,
        max_stream_duration=30,
        retention_days=7,
        db_path=str(tmp_path / "relay.db"),
        host="127.0.0.1",
        port=8000,
        log_level="INFO",
        docs_enabled=False,
        upstream_connect_timeout=1,
        upstream_read_timeout=30,
        upstream_write_timeout=5,
         upstream_pool_timeout=5,
         relay_encryption_key="AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
    )
    return replace(Settings(**values), **overrides)


class FakeResponse:
    status_code = 200

    def __init__(self, chunks: list[bytes], delay: float = 0):
        self.chunks = chunks
        self.delay = delay

    async def aiter_bytes(self) -> AsyncIterator[bytes]:
        for chunk in self.chunks:
            if self.delay:
                await asyncio.sleep(self.delay)
            yield chunk


class FakeUpstream:
    def __init__(
        self,
        stream_chunks: list[bytes] | None = None,
        delay: float = 0,
        model_ids: tuple[str, ...] = ("public-chat",),
    ):
        self.stream_chunks = stream_chunks or [b'data: {"choices":[{"delta":{"content":"ok"}}]}\n\n', b"data: [DONE]\n\n"]
        self.delay = delay
        self.model_ids = model_ids
        self.last_payload: dict[str, Any] | None = None

    async def list_models(self) -> tuple[str, ...]:
        return self.model_ids

    async def post_json(self, payload: dict[str, Any]) -> dict[str, Any]:
        self.last_payload = payload
        return {
            "id": "chatcmpl-test",
            "object": "chat.completion",
            "choices": [{"message": {"role": "assistant", "content": "ok"}, "index": 0}],
            "usage": {"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3},
        }

    @asynccontextmanager
    async def stream(self, payload: dict[str, Any]):
        self.last_payload = payload
        yield FakeResponse(self.stream_chunks, self.delay)

    async def close(self):
        return None


@pytest.fixture
async def app_client(tmp_path):
    settings = build_settings(tmp_path)
    upstream = FakeUpstream()
    application = create_app(settings, upstream=upstream)
    transport = httpx.ASGITransport(app=application)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        yield client, application.state.ledger


@pytest.fixture
async def admin_client(tmp_path):
    settings = build_settings(tmp_path)
    env_path = tmp_path / "relay.env"
    env_path.write_text(serialize_env_mapping(settings.to_env_mapping()), encoding="utf-8")
    application = create_app(settings, upstream=FakeUpstream(), config_store=ConfigStore(env_path))
    transport = httpx.ASGITransport(app=application)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as client:
        yield client, application, env_path


def auth() -> dict[str, str]:
    return {"Authorization": "Bearer public-test"}


def admin_headers(key: str = "admin-test") -> dict[str, str]:
    return {"X-Admin-Key": key}


def payload(**overrides):
    value = {"model": "public-chat", "messages": [{"role": "user", "content": "hello"}], "stream": False}
    value.update(overrides)
    return value
