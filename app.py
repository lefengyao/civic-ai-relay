"""FastAPI application for the Civic Relay service."""

from __future__ import annotations

import asyncio
import ctypes
from ctypes import wintypes
import json
import logging
import math
import os
import sys
import time
import uuid
from contextlib import asynccontextmanager, suppress
from datetime import datetime, timedelta, timezone
from typing import Any, AsyncIterator
from zoneinfo import ZoneInfo

from fastapi import FastAPI, Request
from fastapi.responses import HTMLResponse, JSONResponse, StreamingResponse

from admin_api import AdminService, make_admin_router
from admin_ui import ADMIN_HTML
from config import Settings, SettingsError
from config_store import ConfigStore, ensure_managed_config
from db import Ledger, QuotaExceeded, Reservation
from limiter import ConcurrencyGuard, estimate_tokens
from limiter import KeyConcurrencyRegistry
from runtime import RuntimeConfig, RuntimeLease
from tenant_store import TenantStore
from provider_registry import ProviderRegistry
from upstream import SSEEvent, UpstreamError, UpstreamTimeout, build_upstream, parse_sse_event


UTC = timezone.utc
try:
    BJ = ZoneInfo("Asia/Shanghai")
except Exception:
    # Windows installations may not ship the IANA database; Beijing has no DST.
    BJ = timezone(timedelta(hours=8), name="Asia/Shanghai")
logger = logging.getLogger("civic-relay")
_WINDOWS_MEMORY_API: tuple[Any, Any] | None = None


class _ProcessMemoryCounters(ctypes.Structure):
    _fields_ = [
        ("cb", ctypes.c_ulong),
        ("PageFaultCount", ctypes.c_ulong),
        ("PeakWorkingSetSize", ctypes.c_size_t),
        ("WorkingSetSize", ctypes.c_size_t),
        ("QuotaPeakPagedPoolUsage", ctypes.c_size_t),
        ("QuotaPagedPoolUsage", ctypes.c_size_t),
        ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t),
        ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
        ("PagefileUsage", ctypes.c_size_t),
        ("PeakPagefileUsage", ctypes.c_size_t),
    ]


def process_memory_mb() -> float | None:
    """Return the current process resident memory in megabytes when available."""
    if os.name == "nt":
        global _WINDOWS_MEMORY_API
        counters = _ProcessMemoryCounters()
        counters.cb = ctypes.sizeof(counters)
        try:
            if _WINDOWS_MEMORY_API is None:
                process = ctypes.windll.kernel32.GetCurrentProcess
                process.restype = wintypes.HANDLE
                get_memory_info = ctypes.WinDLL("psapi", use_last_error=True).GetProcessMemoryInfo
                get_memory_info.argtypes = [wintypes.HANDLE, ctypes.POINTER(_ProcessMemoryCounters), wintypes.DWORD]
                get_memory_info.restype = wintypes.BOOL
                _WINDOWS_MEMORY_API = process, get_memory_info
            process, get_memory_info = _WINDOWS_MEMORY_API
            ok = get_memory_info(process(), ctypes.byref(counters), counters.cb)
        except (AttributeError, OSError):
            return None
        return counters.WorkingSetSize / (1024 * 1024) if ok else None

    try:
        # Linux exposes current RSS directly; unlike ru_maxrss this does not
        # permanently trip the guard after a transient allocation.
        with open("/proc/self/statm", encoding="ascii") as handle:
            statm = handle.read().split()
        if len(statm) >= 2:
            return int(statm[1]) * os.sysconf("SC_PAGE_SIZE") / (1024 * 1024)
    except (FileNotFoundError, OSError, ValueError):
        pass
    try:
        import resource

        value = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
        return value / (1024 * 1024) if sys.platform == "darwin" else value / 1024
    except (ImportError, OSError, ValueError):
        return None


def memory_limit_exceeded(settings: Settings) -> bool:
    current = process_memory_mb()
    return current is not None and current > settings.memory_limit_mb


def memory_error(request_id: str, limit_mb: int) -> JSONResponse:
    response = openai_error(
        f"process memory limit exceeded ({limit_mb} MB)",
        503,
        "server_error",
        "memory_limit_exceeded",
        request_id,
    )
    response.headers["Retry-After"] = "5"
    return response


def sse_memory_error(request_id: str, limit_mb: int) -> bytes:
    payload = {
        "error": {
            "message": f"process memory limit exceeded ({limit_mb} MB)",
            "type": "server_error",
            "code": "memory_limit_exceeded",
            "request_id": request_id,
        }
    }
    return f"data: {json.dumps(payload, ensure_ascii=False, separators=(',', ':'))}\n\n".encode("utf-8")


def utc_now() -> datetime:
    return datetime.now(UTC)


def request_id_from(request: Request) -> str:
    candidate = request.headers.get("X-Request-ID", "").strip()
    if len(candidate) <= 64 and candidate and all(0x20 <= ord(char) <= 0x7E for char in candidate):
        return candidate
    return str(uuid.uuid4())


def openai_error(message: str, status: int, error_type: str, code: str, request_id: str) -> JSONResponse:
    return JSONResponse(
        {"error": {"message": message, "type": error_type, "param": None, "code": code}},
        status_code=status,
        headers={"X-Request-ID": request_id},
    )


def _bearer(request: Request) -> str:
    value = request.headers.get("Authorization", "")
    return value[7:].strip() if value.startswith("Bearer ") else ""


def _input_estimate(messages: Any) -> int:
    try:
        text = json.dumps(messages, ensure_ascii=False, separators=(",", ":"))
    except (TypeError, ValueError):
        text = str(messages)
    return estimate_tokens(text)


def _usage_values(usage: Any) -> tuple[int | None, int | None, int | None]:
    if not isinstance(usage, dict):
        return None, None, None
    prompt = usage.get("prompt_tokens")
    completion = usage.get("completion_tokens")
    total = usage.get("total_tokens")
    prompt = int(prompt) if isinstance(prompt, (int, float)) and prompt >= 0 else None
    completion = int(completion) if isinstance(completion, (int, float)) and completion >= 0 else None
    total = int(total) if isinstance(total, (int, float)) and total >= 0 else None
    if total is None and prompt is not None and completion is not None:
        total = prompt + completion
    return prompt, completion, total


def _response_output_characters(payload: dict[str, Any]) -> int:
    total = 0
    for choice in payload.get("choices", []):
        if not isinstance(choice, dict):
            continue
        message = choice.get("message") or {}
        content = message.get("content") if isinstance(message, dict) else None
        if isinstance(content, str):
            total += len(content)
    return total


def _effective_output_tokens(payload: dict[str, Any], settings: Settings) -> tuple[int | None, str | None]:
    values: list[tuple[str, Any]] = []
    for name in ("max_tokens", "max_completion_tokens"):
        if name in payload:
            value = payload[name]
            if isinstance(value, bool) or not isinstance(value, int) or value < 1:
                return None, "invalid output token limit"
            values.append((name, value))
    if len(values) == 2 and values[0][1] != values[1][1]:
        return None, "max_tokens and max_completion_tokens conflict"
    value = values[-1][1] if values else settings.max_output_tokens
    if value > settings.max_output_tokens:
        return None, "max_tokens exceeds server limit"
    return value, None


def create_app(
    settings: Settings | None = None,
    *,
    upstream: Any | None = None,
    ledger: Ledger | None = None,
    guard: ConcurrencyGuard | None = None,
    config_store: ConfigStore | None = None,
) -> FastAPI:
    if settings is None:
        try:
            settings = Settings.from_env()
        except SettingsError:
            # A brand-new installation can start without a provider. Generate
            # the external config once; invalid existing configs still fail fast.
            config_path = ConfigStore().path
            if config_path.exists():
                settings, created = ensure_managed_config(config_path)
            elif os.getenv("ADMIN_API_KEY") or os.getenv("PUBLIC_API_KEY"):
                raise
            else:
                settings, created = ensure_managed_config(config_path)
            if created:
                logger.warning("initial configuration created at %s; read bootstrap-admin-key.txt once", config_path.parent)
    ledger = ledger or Ledger(settings.db_path, settings.retention_days)
    upstream = upstream or build_upstream(settings)
    guard = guard or ConcurrencyGuard(settings.global_concurrency_limit)
    runtime = RuntimeConfig(settings, upstream, guard)
    store = config_store or ConfigStore()
    tenant_store = TenantStore(settings.db_path, settings.relay_encryption_key)
    tenant_store.initialize_legacy_data_sync(settings)
    provider_registry = ProviderRegistry(tenant_store)
    key_guard = KeyConcurrencyRegistry()
    admin_service = AdminService(runtime, store, ledger, guard, utc_now(), tenant_store, provider_registry)

    async def sync_models_forever() -> None:
        """Periodically refresh the local model allowlist without blocking startup."""
        while True:
            current = runtime.settings
            if current.model_auto_sync:
                try:
                    await admin_service.sync_models()
                except (UpstreamError, OSError, RuntimeError):
                    # 上游暂不可用时沿用已保存的白名单，避免影响正在服务的请求。
                    logger.warning("model synchronization failed; retaining the existing allowlist")
                # Legacy settings remain compatible, while independently managed
                # providers are synchronized without one provider blocking another.
                for provider in await tenant_store.list_providers():
                    if provider.name == "legacy" or not provider.enabled:
                        continue
                    try:
                        await provider_registry.sync_provider(provider.id)
                    except (UpstreamError, OSError, RuntimeError, KeyError):
                        logger.warning("provider model synchronization failed; provider_id=%s", provider.id)
                delay_seconds = current.model_sync_interval * 60
            else:
                # 自动同步关闭时仍定期读取设置，以便热加载后无需重启即可恢复。
                delay_seconds = 60
            await asyncio.sleep(delay_seconds)

    @asynccontextmanager
    async def lifespan(_app: FastAPI):
        await ledger.prune()
        model_sync_task = asyncio.create_task(sync_models_forever(), name="civic-relay-model-sync")
        try:
            yield
        finally:
            model_sync_task.cancel()
            with suppress(asyncio.CancelledError):
                await model_sync_task
            await runtime.close()
            await provider_registry.close()
            await tenant_store.close()
            await ledger.close()

    app = FastAPI(
        docs_url="/docs" if settings.docs_enabled else None,
        redoc_url="/redoc" if settings.docs_enabled else None,
        openapi_url="/openapi.json" if settings.docs_enabled else None,
        lifespan=lifespan,
    )
    app.state.settings = settings
    app.state.ledger = ledger
    app.state.runtime = runtime
    app.state.guard = guard
    app.state.admin_service = admin_service
    app.state.tenant_store = tenant_store
    app.state.provider_registry = provider_registry
    app.state.key_guard = key_guard
    app.include_router(make_admin_router(admin_service))

    @app.get("/admin", include_in_schema=False)
    async def admin_shell() -> HTMLResponse:
        return HTMLResponse(ADMIN_HTML)

    @app.middleware("http")
    async def request_middleware(request: Request, call_next):
        request.state.request_id = request_id_from(request)
        current_settings = runtime.settings
        # Keep /admin reachable so an administrator can raise the limit after
        # an overload. Public model endpoints fail fast before reading a body.
        if request.url.path.startswith("/v1/") and memory_limit_exceeded(current_settings):
            return memory_error(request.state.request_id, current_settings.memory_limit_mb)
        content_length = request.headers.get("content-length")
        if content_length:
            try:
                too_large = int(content_length) > current_settings.max_body_bytes
            except ValueError:
                too_large = True
            if too_large:
                return openai_error("request body too large", 413, "invalid_request_error", "body_too_large", request.state.request_id)
        if request.method == "POST" and request.url.path == "/v1/chat/completions":
            body = await request.body()
            if len(body) > current_settings.max_body_bytes:
                return openai_error("request body too large", 413, "invalid_request_error", "body_too_large", request.state.request_id)
        response = await call_next(request)
        response.headers.setdefault("X-Request-ID", request.state.request_id)
        return response

    @app.get("/healthz")
    async def healthz():
        if memory_limit_exceeded(runtime.settings):
            return memory_error("healthz", runtime.settings.memory_limit_mb)
        try:
            await ledger.healthcheck()
        except Exception:
            return JSONResponse({"status": "error"}, status_code=503)
        return {"status": "ok"}

    @app.get("/v1/models")
    async def models(request: Request):
        rid = request.state.request_id
        lease = await runtime.acquire()
        try:
            tenant_store.initialize_legacy_data_sync(lease.settings)
            token = _bearer(request)
            key = await tenant_store.authenticate_key(token)
            if key is None:
                return openai_error("authentication required", 401, "authentication_error", "invalid_api_key", rid)
            authorized = await tenant_store.authorized_models(token)
            return {
                "object": "list",
                "data": [
                    {"id": model.public_name, "object": "model", "created": 0, "owned_by": "relay"}
                    for model in authorized
                ],
            }
        finally:
            await runtime.release(lease)

    @app.post("/v1/chat/completions")
    async def chat_completions(request: Request):
        rid = request.state.request_id
        lease = await runtime.acquire()
        settings = lease.settings
        upstream = lease.upstream
        try:
            tenant_store.initialize_legacy_data_sync(settings)
            token = _bearer(request)
            key = await tenant_store.authenticate_key(token)
            if key is None:
                return openai_error("authentication required", 401, "authentication_error", "invalid_api_key", rid)
            try:
                payload = await request.json()
            except (json.JSONDecodeError, UnicodeDecodeError, ValueError):
                return openai_error("invalid JSON body", 400, "invalid_request_error", "invalid_json", rid)
            if not isinstance(payload, dict):
                return openai_error("request body must be a JSON object", 400, "invalid_request_error", "invalid_request", rid)
            model = payload.get("model")
            messages = payload.get("messages")
            model_record = await tenant_store.provider_model(str(model), token) if isinstance(model, str) else None
            if model_record is None:
                return openai_error("model not allowed", 400, "invalid_request_error", "model_not_allowed", rid)
            if not isinstance(messages, list) or not messages:
                return openai_error("messages must be a non-empty array", 400, "invalid_request_error", "invalid_messages", rid)
            stream = payload.get("stream", False)
            if not isinstance(stream, bool):
                return openai_error("stream must be boolean", 400, "invalid_request_error", "invalid_stream", rid)
            effective_output, token_error = _effective_output_tokens(payload, settings)
            if token_error:
                code = "max_output_tokens_exceeded" if "exceeds" in token_error else "invalid_output_tokens"
                return openai_error(token_error, 400, "invalid_request_error", code, rid)
            input_estimate = _input_estimate(messages)
            reservation_tokens = input_estimate + int(effective_output)
            started = utc_now()
            billing_date = started.astimezone(BJ).date().isoformat()
            reservation: Reservation | None = None
            key_reservation: Reservation | None = None
            acquired = False
            key_acquired = False
            stream_started = False
            began = time.monotonic()
            try:
                reservation = await ledger.reserve(
                    rid,
                    started,
                    billing_date,
                    model,
                    stream,
                    input_estimate,
                    reservation_tokens,
                    settings.rpm_limit,
                    settings.token_limit_5h,
                    settings.token_limit_daily,
                )
                reserved_money = math.ceil((input_estimate * int(model_record.input_price_microyuan_per_million or 0) + int(effective_output) * int(model_record.output_price_microyuan_per_million or 0)) / 1_000_000)
                key_reservation = await tenant_store.reserve_for_key(key.id, model_record.id, input_estimate, int(effective_output), reservation_tokens, reserved_money)
                acquired = await guard.try_acquire()
                if not acquired:
                    await tenant_store.settle_for_key(key_reservation.id, input_estimate, 0, 0, 0)
                    await ledger.cancel(reservation.id, utc_now(), http_status=429, duration_ms=int((time.monotonic() - began) * 1000))
                    return openai_error("concurrency limit exceeded", 429, "rate_limit_error", "concurrency_limit_exceeded", rid)
                key_acquired = await key_guard.try_acquire(key.id, key.max_concurrency)
                if not key_acquired:
                    await tenant_store.settle_for_key(key_reservation.id, input_estimate, 0, 0, 0)
                    await ledger.cancel(reservation.id, utc_now(), http_status=429, duration_ms=int((time.monotonic() - began) * 1000))
                    return openai_error("key concurrency limit exceeded", 429, "rate_limit_error", "key_concurrency_limit_exceeded", rid)
                upstream_payload = dict(payload)
                upstream_payload["model"] = model_record.upstream_name
                upstream_payload["max_tokens"] = effective_output
                request_upstream = lease.upstream
                legacy_provider = next((p for p in await tenant_store.list_providers() if p.name == "legacy"), None)
                if legacy_provider is None or model_record.provider_id != legacy_provider.id:
                    request_upstream = await provider_registry.client_for_model(model_record.id)
                if stream:
                    response = StreamingResponse(
                        stream_response(request, settings, request_upstream, lease, upstream_payload, reservation, input_estimate, rid, tenant_store, key_reservation, key_guard, key_acquired, int(model_record.input_price_microyuan_per_million or 0), int(model_record.output_price_microyuan_per_million or 0)),
                        media_type="text/event-stream",
                        headers={"Cache-Control": "no-cache", "Connection": "keep-alive"},
                    )
                    stream_started = True
                    return response
                actual = await request_upstream.post_json(upstream_payload)
                prompt, completion, total = _usage_values(actual.get("usage"))
                if total is None:
                    completion = max(1, (_response_output_characters(actual) + 1) // 2)
                    prompt = input_estimate
                    total = prompt + completion
                charged_money = math.ceil((int(prompt or input_estimate) * int(model_record.input_price_microyuan_per_million or 0) + int(completion or 0) * int(model_record.output_price_microyuan_per_million or 0)) / 1_000_000)
                if key_reservation:
                    await tenant_store.settle_for_key(key_reservation.id, int(prompt or input_estimate), int(completion or 0), int(total), charged_money)
                await ledger.settle(reservation.id, utc_now(), "completed", prompt, completion, total, 200, int((time.monotonic() - began) * 1000))
                logger.info("request_id=%s model=%s stream=%s status=completed charged_tokens=%s duration_ms=%s", rid, model, stream, total, int((time.monotonic() - began) * 1000))
                return JSONResponse(actual, headers={"X-Request-ID": rid})
            except QuotaExceeded as exc:
                if reservation:
                    with suppress(Exception):
                        await ledger.cancel(reservation.id, utc_now(), http_status=429, duration_ms=int((time.monotonic() - began) * 1000))
                if exc.code == "rpm_exceeded":
                    code, message = "rpm_exceeded", "rate limit exceeded"
                elif exc.code in {"key_token_quota_exceeded", "key_money_quota_exceeded"}:
                    code, message = exc.code, "client key quota exceeded"
                elif exc.code == "key_disabled":
                    code, message = "key_disabled", "client key disabled"
                elif exc.code == "model_unavailable":
                    code, message = "model_unavailable", "model unavailable"
                else:
                    code, message = "token_quota_exceeded", "token quota exceeded"
                return openai_error(message, 429 if code != "model_unavailable" else 400, "rate_limit_error" if code != "model_unavailable" else "invalid_request_error", code, rid)
            except UpstreamTimeout:
                if key_reservation:
                    with suppress(Exception): await tenant_store.settle_for_key(key_reservation.id, input_estimate, 0, reservation.reserved_tokens if reservation else 0, key_reservation.reserved_money_microyuan)
                if reservation:
                    await ledger.settle(reservation.id, utc_now(), "failed", input_estimate, None, reservation.reserved_tokens, 504, int((time.monotonic() - began) * 1000))
                return openai_error("upstream timeout", 504, "upstream_error", "upstream_timeout", rid)
            except UpstreamError as exc:
                if key_reservation:
                    with suppress(Exception): await tenant_store.settle_for_key(key_reservation.id, input_estimate, 0, reservation.reserved_tokens if reservation else 0, key_reservation.reserved_money_microyuan)
                if reservation:
                    await ledger.settle(reservation.id, utc_now(), "failed", input_estimate, None, reservation.reserved_tokens, 502, int((time.monotonic() - began) * 1000))
                if exc.status_code == 429:
                    status = 429
                elif exc.status_code and 400 <= exc.status_code < 500 and exc.code == "upstream_client_error":
                    status = exc.status_code
                else:
                    status = 502
                return openai_error(str(exc), status, "upstream_error", exc.code, rid)
            except Exception:
                if key_reservation:
                    with suppress(Exception): await tenant_store.settle_for_key(key_reservation.id, input_estimate, 0, reservation.reserved_tokens if reservation else 0, key_reservation.reserved_money_microyuan)
                if reservation:
                    await ledger.settle(reservation.id, utc_now(), "failed", input_estimate, None, reservation.reserved_tokens, 500, int((time.monotonic() - began) * 1000))
                logger.exception("request_id=%s stream=%s status=failed", rid, stream)
                return openai_error("internal server error", 500, "server_error", "internal_error", rid)
            finally:
                if not stream_started:
                    try:
                        if acquired:
                            await guard.release()
                        if key_acquired:
                            await key_guard.release(key.id)
                    finally:
                        await runtime.release(lease)
        finally:
            # Requests rejected during parsing/authentication still own a lease.
            if "stream_started" not in locals():
                await runtime.release(lease)

    async def stream_response(
        request: Request,
        request_settings: Settings,
        request_upstream: Any,
        runtime_lease: RuntimeLease,
        payload: dict[str, Any],
        reservation: Reservation,
        input_estimate: int,
        rid: str,
        request_tenant_store: TenantStore | None = None,
        key_reservation: Reservation | None = None,
        request_key_guard: KeyConcurrencyRegistry | None = None,
        key_acquired: bool = False,
        input_price_microyuan_per_million: int = 0,
        output_price_microyuan_per_million: int = 0,
    ) -> AsyncIterator[bytes]:
        output_characters = 0
        final_usage: dict[str, Any] | None = None
        status = "completed"
        http_status = 200
        started = time.monotonic()
        buffer = b""
        try:
            async with request_upstream.stream(payload) as response:
                async for chunk in response.aiter_bytes():
                    if memory_limit_exceeded(request_settings):
                        status = "failed"
                        http_status = 503
                        yield sse_memory_error(rid, request_settings.memory_limit_mb)
                        break
                    if await request.is_disconnected():
                        status = "aborted"
                        http_status = 499
                        break
                    buffer += chunk
                    while b"\n\n" in buffer:
                        event_bytes, buffer = buffer.split(b"\n\n", 1)
                        event: SSEEvent = parse_sse_event(event_bytes)
                        output_characters += event.output_characters
                        if event.usage:
                            final_usage = event.usage
                    yield chunk
                    if time.monotonic() - started >= request_settings.max_stream_duration:
                        status = "failed"
                        http_status = 504
                        break
        except UpstreamTimeout:
            status = "failed"
            http_status = 504
        except UpstreamError:
            status = "failed"
            http_status = 502
        except Exception:
            status = "failed"
            http_status = 502
        finally:
            if buffer:
                event = parse_sse_event(buffer)
                output_characters += event.output_characters
                if event.usage:
                    final_usage = event.usage
            prompt, completion, total = _usage_values(final_usage)
            if total is None:
                completion = max(1, (output_characters + 1) // 2)
                prompt = input_estimate
                total = prompt + completion
                if output_characters == 0 and status != "completed":
                    total = reservation.reserved_tokens
            await ledger.settle(
                reservation.id,
                utc_now(),
                status,
                prompt,
                completion,
                total,
                http_status,
                int((time.monotonic() - started) * 1000),
            )
            logger.info("request_id=%s stream=true status=%s charged_tokens=%s", rid, status, total)
            if request_tenant_store is not None and key_reservation is not None:
                with suppress(Exception):
                    charged_money = math.ceil((int(prompt or input_estimate) * input_price_microyuan_per_million + int(completion or 0) * output_price_microyuan_per_million) / 1_000_000)
                    await request_tenant_store.settle_for_key(key_reservation.id, int(prompt or input_estimate), int(completion or 0), int(total), charged_money)
            try:
                await guard.release()
            finally:
                if request_key_guard is not None and key_acquired:
                    await request_key_guard.release(key_reservation.client_key_id if key_reservation else "")
                await runtime.release(runtime_lease)

    return app


app = create_app()
