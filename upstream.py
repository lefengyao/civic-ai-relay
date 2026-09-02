"""HTTP transport and lightweight SSE accounting for an OpenAI-compatible upstream."""

from __future__ import annotations

import json
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, AsyncIterator

import httpx

if TYPE_CHECKING:
    from config import Settings


@dataclass(frozen=True)
class SSEEvent:
    output_characters: int = 0
    usage: dict[str, Any] | None = None
    done: bool = False


def parse_sse_event(event: bytes | str) -> SSEEvent:
    text = event.decode("utf-8", errors="replace") if isinstance(event, bytes) else event
    output_characters = 0
    usage: dict[str, Any] | None = None
    done = False
    for line in text.splitlines():
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if payload == "[DONE]":
            done = True
            continue
        try:
            data = json.loads(payload)
        except (TypeError, ValueError, json.JSONDecodeError):
            continue
        if not isinstance(data, dict):
            continue
        candidate_usage = data.get("usage")
        if isinstance(candidate_usage, dict):
            usage = candidate_usage
        for choice in data.get("choices", []):
            if not isinstance(choice, dict):
                continue
            delta = choice.get("delta") or {}
            if isinstance(delta, dict) and isinstance(delta.get("content"), str):
                output_characters += len(delta["content"])
    return SSEEvent(output_characters=output_characters, usage=usage, done=done)


class UpstreamError(RuntimeError):
    def __init__(self, status_code: int | None, code: str, message: str):
        self.status_code = status_code
        self.code = code
        super().__init__(message)


class UpstreamTimeout(UpstreamError):
    def __init__(self):
        super().__init__(None, "upstream_timeout", "upstream timeout")


class UnconfiguredUpstream:
    """Placeholder transport used before an administrator adds a provider."""

    async def close(self) -> None:
        return None

    async def list_models(self) -> tuple[str, ...]:
        raise UpstreamError(None, "no_upstream_configured", "no upstream provider configured")

    async def post_json(self, payload: dict[str, Any]) -> dict[str, Any]:
        raise UpstreamError(None, "no_upstream_configured", "no upstream provider configured")

    @asynccontextmanager
    async def stream(self, payload: dict[str, Any]) -> AsyncIterator[Any]:
        raise UpstreamError(None, "no_upstream_configured", "no upstream provider configured")
        yield  # pragma: no cover


class UpstreamClient:
    def __init__(
        self,
        base_url: str,
        api_key: str,
        connect_timeout: float = 10,
        read_timeout: float = 300,
        write_timeout: float = 30,
        pool_timeout: float = 10,
        client: httpx.AsyncClient | None = None,
    ):
        self.base_url = base_url.rstrip("/")
        self.url = self.base_url + "/v1/chat/completions"
        self.models_url = self.base_url + "/v1/models"
        self._api_key = api_key
        self._owns_client = client is None
        self.client = client or httpx.AsyncClient(
            timeout=httpx.Timeout(
                connect=connect_timeout,
                read=read_timeout,
                write=write_timeout,
                pool=pool_timeout,
            )
        )

    @property
    def headers(self) -> dict[str, str]:
        return {"Authorization": f"Bearer {self._api_key}"}

    @property
    def api_key(self) -> str:
        """Read-only credential accessor for the provider registry/tests.

        Callers must never serialize this value into API responses or logs.
        """
        return self._api_key

    async def close(self) -> None:
        if self._owns_client:
            await self.client.aclose()

    @staticmethod
    def with_usage_option(payload: dict[str, Any]) -> dict[str, Any]:
        updated = dict(payload)
        options = updated.get("stream_options")
        options = dict(options) if isinstance(options, dict) else {}
        options["include_usage"] = True
        updated["stream_options"] = options
        return updated

    @staticmethod
    def _raise_for_status(response: httpx.Response) -> None:
        if response.status_code < 400:
            return
        if response.status_code in {401, 403}:
            raise UpstreamError(response.status_code, "upstream_authentication_failed", "upstream authentication failed")
        if response.status_code == 429:
            raise UpstreamError(429, "upstream_rate_limit", "upstream rate limit")
        if response.status_code >= 500:
            raise UpstreamError(response.status_code, "upstream_server_error", "upstream server error")
        raise UpstreamError(response.status_code, "upstream_client_error", "upstream request rejected")

    async def post_json(self, payload: dict[str, Any]) -> dict[str, Any]:
        try:
            response = await self.client.post(self.url, headers=self.headers, json=payload)
        except (httpx.TimeoutException, httpx.NetworkError) as exc:
            if isinstance(exc, httpx.TimeoutException):
                raise UpstreamTimeout() from None
            raise UpstreamError(None, "upstream_connection_failed", "upstream connection failed") from None
        self._raise_for_status(response)
        try:
            result = response.json()
        except (ValueError, json.JSONDecodeError):
            raise UpstreamError(response.status_code, "upstream_invalid_json", "upstream returned invalid JSON") from None
        if not isinstance(result, dict):
            raise UpstreamError(response.status_code, "upstream_invalid_json", "upstream returned invalid JSON")
        return result

    async def list_models(self) -> tuple[str, ...]:
        """Fetch the upstream model identifiers for the local allowlist."""
        try:
            response = await self.client.get(self.models_url, headers=self.headers)
        except (httpx.TimeoutException, httpx.NetworkError) as exc:
            if isinstance(exc, httpx.TimeoutException):
                raise UpstreamTimeout() from None
            raise UpstreamError(None, "upstream_connection_failed", "upstream connection failed") from None
        self._raise_for_status(response)
        try:
            payload = response.json()
        except (ValueError, json.JSONDecodeError):
            raise UpstreamError(response.status_code, "upstream_invalid_models", "upstream returned invalid models") from None
        if not isinstance(payload, dict) or not isinstance(payload.get("data"), list):
            raise UpstreamError(response.status_code, "upstream_invalid_models", "upstream returned invalid models")

        # 仅保留可安全写入本地白名单的非空字符串模型 ID，并保持上游顺序。
        model_ids = tuple(
            dict.fromkeys(
                item["id"].strip()
                for item in payload["data"]
                if isinstance(item, dict) and isinstance(item.get("id"), str) and item["id"].strip()
            )
        )
        if not model_ids:
            raise UpstreamError(response.status_code, "upstream_invalid_models", "upstream returned no usable models")
        return model_ids

    @asynccontextmanager
    async def stream(self, payload: dict[str, Any]) -> AsyncIterator[httpx.Response]:
        try:
            async with self.client.stream("POST", self.url, headers=self.headers, json=self.with_usage_option(payload)) as response:
                self._raise_for_status(response)
                yield response
        except (httpx.TimeoutException, httpx.ReadTimeout) as exc:
            raise UpstreamTimeout() from None
        except httpx.NetworkError:
            raise UpstreamError(None, "upstream_connection_failed", "upstream connection failed") from None


def build_upstream(settings: "Settings") -> UpstreamClient:
    """Construct an upstream client from one immutable settings snapshot."""
    if not settings.upstream_base_url or not settings.upstream_api_key:
        return UnconfiguredUpstream()
    return UpstreamClient(
        settings.upstream_base_url,
        settings.upstream_api_key,
        settings.upstream_connect_timeout,
        settings.upstream_read_timeout,
        settings.upstream_write_timeout,
        settings.upstream_pool_timeout,
    )
