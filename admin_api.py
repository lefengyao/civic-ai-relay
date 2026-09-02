"""Internal-only administration API for Civic Relay."""

from __future__ import annotations

import asyncio
import hmac
from dataclasses import dataclass, replace
from datetime import datetime, timedelta, timezone
from typing import Any
from zoneinfo import ZoneInfo

from fastapi import APIRouter, Request
from fastapi.responses import JSONResponse

from config import SettingsError
from config_store import ConfigStore
from db import Ledger
from limiter import ConcurrencyGuard
from runtime import RuntimeConfig
from upstream import UpstreamError
from tenant_store import TenantStore
from provider_registry import ProviderRegistry


UTC = timezone.utc
try:
    BJ = ZoneInfo("Asia/Shanghai")
except Exception:
    BJ = timezone(timedelta(hours=8), name="Asia/Shanghai")


def _error(message: str, status_code: int, request_id: str | None = None) -> JSONResponse:
    headers = {"X-Request-ID": request_id} if request_id else None
    return JSONResponse({"error": {"message": message}}, status_code=status_code, headers=headers)


@dataclass
class AdminService:
    runtime: RuntimeConfig
    store: ConfigStore
    ledger: Ledger
    guard: ConcurrencyGuard
    started_at_utc: datetime
    tenant_store: TenantStore | None = None
    provider_registry: ProviderRegistry | None = None

    def __post_init__(self) -> None:
        self._reload_lock = asyncio.Lock()

    def authorized(self, request: Request) -> bool:
        supplied = request.headers.get("X-Admin-Key", "")
        expected = self.runtime.settings.admin_api_key
        return bool(supplied) and hmac.compare_digest(supplied.encode(), expected.encode())

    async def validate(self, patch: dict[str, str | None]) -> tuple[Any, list[str]]:
        candidate = self.store.build_candidate(patch, self.runtime.managed_settings)
        before = self.runtime.managed_settings.to_env_mapping()
        after = candidate.to_env_mapping()
        changed = [name for name in after if after[name] != before[name]]
        return candidate, changed

    async def save(self, patch: dict[str, str | None]) -> tuple[list[str], Any]:
        async with self._reload_lock:
            candidate, changed = await self.validate(patch)
            prepared = await self.runtime.prepare_reload(candidate)
            try:
                self.store.write(candidate)
            except Exception:
                await self.runtime.discard_prepared_reload(prepared)
                raise
            result = await self.runtime.commit_prepared_reload(prepared)
        return changed, result

    async def sync_models(self) -> tuple[list[str], Any]:
        """Fetch upstream models and atomically apply them as the allowlist."""
        async with self._reload_lock:
            lease = await self.runtime.acquire()
            try:
                model_ids = await lease.upstream.list_models()
            finally:
                await self.runtime.release(lease)

            # 同步仅替换模型白名单，保留管理员已配置的其余运行参数。
            candidate = replace(self.runtime.managed_settings, model_whitelist=model_ids)
            prepared = await self.runtime.prepare_reload(candidate)
            try:
                self.store.write(candidate)
            except Exception:
                await self.runtime.discard_prepared_reload(prepared)
                raise
            result = await self.runtime.commit_prepared_reload(prepared)
        return list(model_ids), result


def _parse_patch(payload: Any, service: AdminService) -> dict[str, str | None]:
    if not isinstance(payload, dict) or not isinstance(payload.get("settings"), dict):
        raise ValueError("settings must be an object")
    names = set(service.runtime.managed_settings.to_env_mapping())
    patch: dict[str, str | None] = {}
    for name, value in payload["settings"].items():
        if name not in names:
            raise ValueError(f"unknown setting: {name}")
        if value is not None and not isinstance(value, str):
            raise ValueError(f"invalid setting: {name}")
        patch[name] = value
    return patch


def make_admin_router(service: AdminService) -> APIRouter:
    router = APIRouter(prefix="/admin/api")

    async def require_admin(request: Request) -> JSONResponse | None:
        if service.authorized(request):
            return None
        return _error("authentication required", 401, getattr(request.state, "request_id", None))

    @router.get("/overview")
    async def overview(request: Request):
        denied = await require_admin(request)
        if denied:
            return denied
        now = datetime.now(UTC)
        try:
            await service.ledger.healthcheck()
            ledger_status = "ok"
        except Exception:
            ledger_status = "error"
        try:
            snapshot = await service.ledger.monitoring_snapshot(now, now.astimezone(BJ).date().isoformat())
        except Exception:
            # Keep the console reachable while exposing the failed ledger state.
            # The next poll will recover automatically if SQLite becomes available.
            ledger_status = "error"
            snapshot = {
                "rpm_count": 0,
                "five_hour_tokens": 0,
                "daily_tokens": 0,
                "last_hour": {
                    "completed": 0,
                    "failed": 0,
                    "aborted": 0,
                    "rejected": 0,
                    "reserved": 0,
                    "error_rate": 0.0,
                },
                "trend": [],
                "recent": [],
            }
        active, limit = await service.guard.snapshot()
        settings = service.runtime.settings
        return {
            "generated_at_utc": now.isoformat(timespec="seconds").replace("+00:00", "Z"),
            "uptime_seconds": max(0, int((now - service.started_at_utc).total_seconds())),
            "ledger_status": ledger_status,
            "concurrency": {"active": active, "limit": limit},
            "rpm": {"used": snapshot["rpm_count"], "limit": settings.rpm_limit},
            "five_hour": {"used_tokens": snapshot["five_hour_tokens"], "limit": settings.token_limit_5h},
            "daily": {"used_tokens": snapshot["daily_tokens"], "limit": settings.token_limit_daily},
            "last_hour": snapshot["last_hour"],
            "trend": snapshot["trend"],
            "recent": snapshot["recent"],
        }

    @router.get("/requests")
    async def requests(request: Request):
        denied = await require_admin(request)
        if denied:
            return denied
        raw_limit = request.query_params.get("limit", "50")
        try:
            limit = int(raw_limit)
        except ValueError:
            return _error("invalid limit", 400, getattr(request.state, "request_id", None))
        if not 1 <= limit <= 50:
            return _error("invalid limit", 400, getattr(request.state, "request_id", None))
        return {"data": await service.ledger.recent_requests(limit)}

    @router.get("/config")
    async def configuration(request: Request):
        denied = await require_admin(request)
        if denied:
            return denied
        managed = service.runtime.managed_settings
        active = service.runtime.settings
        pending = [
            name for name, value in managed.to_env_mapping().items()
            if name in {"HOST", "PORT", "DB_PATH", "DOCS_ENABLED"}
            and value != active.to_env_mapping()[name]
        ]
        return {
            "settings": managed.redacted(),
            "pending_restart_fields": pending,
        }

    @router.post("/config/validate")
    async def validate_configuration(request: Request):
        denied = await require_admin(request)
        if denied:
            return denied
        try:
            patch = _parse_patch(await request.json(), service)
            _, changed = await service.validate(patch)
        except (ValueError, SettingsError):
            return _error("configuration validation failed", 400, getattr(request.state, "request_id", None))
        return {"valid": True, "changed_fields": changed}

    @router.put("/config")
    async def save_configuration(request: Request):
        denied = await require_admin(request)
        if denied:
            return denied
        try:
            patch = _parse_patch(await request.json(), service)
            changed, result = await service.save(patch)
        except (ValueError, SettingsError):
            return _error("configuration validation failed", 400, getattr(request.state, "request_id", None))
        except OSError:
            return _error("configuration save failed", 500, getattr(request.state, "request_id", None))
        except RuntimeError:
            return _error("configuration reload failed", 409, getattr(request.state, "request_id", None))
        return {
            "applied_at_utc": result.applied_at_utc,
            "changed_fields": changed,
            "pending_restart_fields": list(result.pending_restart_fields),
        }

    @router.post("/models/sync")
    async def sync_models(request: Request):
        denied = await require_admin(request)
        if denied:
            return denied
        try:
            models, result = await service.sync_models()
        except (UpstreamError, OSError, RuntimeError):
            return _error("model synchronization failed", 502, getattr(request.state, "request_id", None))
        return {
            "models": models,
            "applied_at_utc": result.applied_at_utc,
        }

    def tenant_or_error() -> TenantStore:
        if service.tenant_store is None:
            raise RuntimeError("tenant store unavailable")
        return service.tenant_store

    @router.get("/providers")
    async def list_providers(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        return {"data": [item.__dict__ for item in await tenant_or_error().list_providers()]}

    @router.post("/providers", status_code=201)
    async def create_provider(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            provider = await tenant_or_error().create_provider(str(payload["name"]), str(payload["base_url"]), str(payload["api_key"]))
        except (KeyError, TypeError, ValueError):
            return _error("invalid provider", 400, getattr(request.state, "request_id", None))
        return {"data": provider.__dict__}

    @router.put("/providers/{provider_id}")
    async def update_provider(provider_id: int, request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            provider = await tenant_or_error().update_provider(
                provider_id, name=payload.get("name"), base_url=payload.get("base_url"),
                api_key=payload.get("api_key"), enabled=payload.get("enabled"),
            )
        except (KeyError, TypeError, ValueError):
            return _error("invalid provider", 400, getattr(request.state, "request_id", None))
        return {"data": provider.__dict__}

    @router.post("/providers/{provider_id}/sync")
    async def sync_provider(provider_id: int, request: Request):
        denied = await require_admin(request)
        if denied: return denied
        if service.provider_registry is None:
            return _error("provider registry unavailable", 503, getattr(request.state, "request_id", None))
        try:
            models, imported = await service.provider_registry.sync_provider(provider_id)
        except (KeyError, UpstreamError, OSError, RuntimeError):
            return _error("provider model synchronization failed", 502, getattr(request.state, "request_id", None))
        return {"models": models, "imported": imported or []}

    @router.get("/models")
    async def list_models(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        return {"data": [item.__dict__ for item in await tenant_or_error().list_models()]}

    @router.post("/models", status_code=201)
    async def create_model(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            def price(name: str, alt: str) -> int | None:
                value = payload.get(name, payload.get(alt))
                return None if value is None else int(round(float(value) * 1_000_000))
            model = await tenant_or_error().create_model(str(payload["public_name"]), int(payload["provider_id"]), str(payload.get("upstream_name", payload["public_name"])), price("input_price_yuan_per_million", "input_price"), price("output_price_yuan_per_million", "output_price"), bool(payload.get("enabled", False)))
        except (KeyError, TypeError, ValueError):
            return _error("invalid model", 400, getattr(request.state, "request_id", None))
        return {"data": model.__dict__}

    @router.put("/models/{model_id}")
    async def update_model(model_id: int, request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            def price(name: str, alt: str) -> int | None:
                if name not in payload and alt not in payload: return None
                value = payload.get(name, payload.get(alt))
                return None if value is None else int(round(float(value) * 1_000_000))
            model = await tenant_or_error().update_model(model_id, public_name=payload.get("public_name"), upstream_name=payload.get("upstream_name"), input_price=price("input_price_yuan_per_million", "input_price"), output_price=price("output_price_yuan_per_million", "output_price"), enabled=payload.get("enabled"))
        except (KeyError, TypeError, ValueError):
            return _error("invalid model", 400, getattr(request.state, "request_id", None))
        return {"data": model.__dict__}

    @router.get("/groups")
    async def list_groups(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        groups = await tenant_or_error().list_model_groups()
        data = []
        for group in groups:
            data.append({**group.__dict__, "models": [model.public_name for model in await tenant_or_error().group_models(group.id)]})
        return {"data": data}

    @router.post("/groups", status_code=201)
    async def create_group(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            group = await tenant_or_error().create_model_group(str(payload["name"]))
            if "model_ids" in payload: await tenant_or_error().set_group_models(group.id, [int(item) for item in payload["model_ids"]])
        except (KeyError, TypeError, ValueError):
            return _error("invalid model group", 400, getattr(request.state, "request_id", None))
        return {"data": group.__dict__}

    @router.put("/groups/{group_id}/models")
    async def update_group_models(group_id: int, request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            await tenant_or_error().set_group_models(group_id, [int(item) for item in payload.get("model_ids", [])])
        except (TypeError, ValueError):
            return _error("invalid model ids", 400, getattr(request.state, "request_id", None))
        return {"ok": True}

    @router.get("/keys")
    async def list_keys(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        store = tenant_or_error()
        data = []
        for key in await store.list_client_keys():
            data.append({**key, "groups": [group.name for group in await store.key_groups(int(key["id"]))]})
        return {"data": data}

    @router.post("/keys", status_code=201)
    async def create_key(request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            money = payload.get("money_limit_microyuan_total")
            if money is None and payload.get("money_limit_yuan_total") is not None:
                money = int(round(float(payload["money_limit_yuan_total"]) * 1_000_000))
            key = await tenant_or_error().create_client_key(str(payload["name"]), int(payload.get("max_concurrency", 1)), payload.get("token_limit_total"), money)
            if "group_ids" in payload: await tenant_or_error().set_key_groups(key.id, [int(item) for item in payload["group_ids"]])
        except (KeyError, TypeError, ValueError):
            return _error("invalid client key", 400, getattr(request.state, "request_id", None))
        return {"data": key.__dict__, "token": key.token}

    @router.put("/keys/{key_id}")
    async def update_key(key_id: int, request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            money = payload.get("money_limit_microyuan_total")
            if money is None and payload.get("money_limit_yuan_total") is not None:
                money = int(round(float(payload["money_limit_yuan_total"]) * 1_000_000))
            key = await tenant_or_error().update_client_key(key_id, max_concurrency=payload.get("max_concurrency"), token_limit_total=payload.get("token_limit_total"), money_limit_microyuan_total=money)
            if "enabled" in payload: key = await tenant_or_error().set_client_key_enabled(key_id, bool(payload["enabled"]), "manual_disabled" if not payload["enabled"] else None)
        except (KeyError, TypeError, ValueError):
            return _error("invalid client key", 400, getattr(request.state, "request_id", None))
        return {"data": key.__dict__}

    @router.put("/keys/{key_id}/groups")
    async def update_key_groups(key_id: int, request: Request):
        denied = await require_admin(request)
        if denied: return denied
        try:
            payload = await request.json()
            await tenant_or_error().set_key_groups(key_id, [int(item) for item in payload.get("group_ids", [])])
        except (TypeError, ValueError):
            return _error("invalid group ids", 400, getattr(request.state, "request_id", None))
        return {"ok": True}

    return router
