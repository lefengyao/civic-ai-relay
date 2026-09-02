"""SQLite-backed multi-tenant resources and client-key authentication."""

from __future__ import annotations

import asyncio
import base64
import hashlib
import json
import secrets
import sqlite3
import threading
import uuid
import math
from datetime import datetime, timezone
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from cryptography.fernet import Fernet, InvalidToken

from db import QuotaExceeded, Reservation


def _fernet(master_key: str) -> Fernet:
    """Build a stable Fernet key from the deployment-only master secret."""
    try:
        return Fernet(master_key.encode("ascii"))
    except (ValueError, UnicodeEncodeError):
        digest = hashlib.sha256(master_key.encode("utf-8")).digest()
        return Fernet(base64.urlsafe_b64encode(digest))


def _digest(token: str) -> str:
    return hashlib.sha256(token.encode("utf-8")).hexdigest()


@dataclass(frozen=True)
class ProviderRecord:
    id: int
    name: str
    base_url: str
    enabled: bool


@dataclass(frozen=True)
class ModelRecord:
    id: int
    public_name: str
    provider_id: int
    upstream_name: str
    input_price_microyuan_per_million: int | None
    output_price_microyuan_per_million: int | None
    enabled: bool

    @property
    def priced(self) -> bool:
        return self.input_price_microyuan_per_million is not None and self.output_price_microyuan_per_million is not None


@dataclass(frozen=True)
class ModelGroupRecord:
    id: int
    name: str


@dataclass(frozen=True)
class ClientKeyRecord:
    id: int
    name: str
    token: str | None
    key_prefix: str
    enabled: bool
    max_concurrency: int
    token_limit_total: int | None
    money_limit_microyuan_total: int | None
    token_used: int = 0
    money_used_microyuan: int = 0
    disabled_reason: str | None = None


class TenantStore:
    """Persist providers, models, groups, keys, and key-level quota state."""

    def __init__(self, db_path: str | Path, encryption_key: str):
        if not encryption_key or not encryption_key.strip():
            raise ValueError("encryption_key is required")
        self.db_path = Path(db_path)
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._cipher = _fernet(encryption_key.strip())
        self._lock = threading.Lock()
        self._initialize()

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(str(self.db_path), timeout=5.0)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA busy_timeout=5000")
        conn.execute("PRAGMA foreign_keys=ON")
        conn.execute("PRAGMA synchronous=NORMAL")
        return conn

    def _initialize(self) -> None:
        with self._lock, self._connect() as conn:
            conn.execute("PRAGMA journal_mode=WAL")
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS providers (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    name TEXT NOT NULL UNIQUE,
                    base_url TEXT NOT NULL,
                    encrypted_api_key TEXT NOT NULL,
                    enabled INTEGER NOT NULL DEFAULT 1,
                    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
                    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
                );
                CREATE TABLE IF NOT EXISTS models (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    public_name TEXT NOT NULL UNIQUE,
                    provider_id INTEGER NOT NULL REFERENCES providers(id),
                    upstream_name TEXT NOT NULL,
                    input_price_microyuan_per_million INTEGER,
                    output_price_microyuan_per_million INTEGER,
                    enabled INTEGER NOT NULL DEFAULT 0,
                    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
                    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
                );
                CREATE INDEX IF NOT EXISTS idx_models_provider ON models(provider_id);
                CREATE TABLE IF NOT EXISTS model_groups (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    name TEXT NOT NULL UNIQUE,
                    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
                    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
                );
                CREATE TABLE IF NOT EXISTS model_group_members (
                    group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
                    model_id INTEGER NOT NULL REFERENCES models(id) ON DELETE CASCADE,
                    PRIMARY KEY (group_id, model_id)
                );
                CREATE TABLE IF NOT EXISTS client_keys (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    name TEXT NOT NULL UNIQUE,
                    key_prefix TEXT NOT NULL UNIQUE,
                    key_digest TEXT NOT NULL UNIQUE,
                    enabled INTEGER NOT NULL DEFAULT 1,
                    max_concurrency INTEGER NOT NULL,
                    token_limit_total INTEGER,
                    money_limit_microyuan_total INTEGER,
                    token_used INTEGER NOT NULL DEFAULT 0,
                    money_used_microyuan INTEGER NOT NULL DEFAULT 0,
                    disabled_reason TEXT,
                    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
                    updated_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
                );
                CREATE TABLE IF NOT EXISTS client_key_groups (
                    key_id INTEGER NOT NULL REFERENCES client_keys(id) ON DELETE CASCADE,
                    group_id INTEGER NOT NULL REFERENCES model_groups(id) ON DELETE CASCADE,
                    PRIMARY KEY (key_id, group_id)
                );
                CREATE TABLE IF NOT EXISTS key_reservations (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    request_id TEXT NOT NULL,
                    started_at_utc TEXT NOT NULL,
                    finished_at_utc TEXT,
                    billing_date_bj TEXT NOT NULL,
                    model_id INTEGER NOT NULL REFERENCES models(id),
                    status TEXT NOT NULL,
                    reserved_tokens INTEGER NOT NULL DEFAULT 0,
                    input_tokens INTEGER,
                    output_tokens INTEGER,
                    charged_tokens INTEGER NOT NULL DEFAULT 0,
                    client_key_id INTEGER,
                    reserved_money_microyuan INTEGER NOT NULL DEFAULT 0,
                    charged_money_microyuan INTEGER NOT NULL DEFAULT 0,
                    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
                );
                CREATE INDEX IF NOT EXISTS idx_key_reservations_client_key ON key_reservations(client_key_id);
                """
            )

    async def close(self) -> None:
        await asyncio.sleep(0)

    def initialize_legacy_data_sync(self, settings: Any) -> None:
        """Idempotently import the single-tenant settings into tenant tables."""
        if not getattr(settings, "upstream_base_url", "").strip() or not getattr(settings, "upstream_api_key", "").strip() or not getattr(settings, "model_whitelist", ()):
            return
        with self._lock, self._connect() as conn:
            provider = conn.execute("SELECT * FROM providers WHERE name=?", ("legacy",)).fetchone()
            if provider is None:
                encrypted = self._cipher.encrypt(settings.upstream_api_key.encode()).decode("ascii")
                cur = conn.execute("INSERT INTO providers(name, base_url, encrypted_api_key, enabled) VALUES ('legacy', ?, ?, 1)", (settings.upstream_base_url, encrypted))
                provider_id = int(cur.lastrowid)
            else:
                provider_id = int(provider["id"])
            model_ids: list[int] = []
            for public_name in settings.model_whitelist:
                upstream_name = settings.model_map.get(public_name, public_name)
                row = conn.execute("SELECT * FROM models WHERE public_name=?", (public_name,)).fetchone()
                if row is None:
                    cur = conn.execute("INSERT INTO models(public_name, provider_id, upstream_name, input_price_microyuan_per_million, output_price_microyuan_per_million, enabled) VALUES (?, ?, ?, 0, 0, 1)", (public_name, provider_id, upstream_name))
                    model_id = int(cur.lastrowid)
                else:
                    model_id = int(row["id"])
                model_ids.append(model_id)
            group = conn.execute("SELECT * FROM model_groups WHERE name='默认组'").fetchone()
            group_id = int(group["id"]) if group else int(conn.execute("INSERT INTO model_groups(name) VALUES ('默认组')").lastrowid)
            conn.execute("DELETE FROM model_group_members WHERE group_id=?", (group_id,))
            conn.executemany("INSERT OR IGNORE INTO model_group_members(group_id, model_id) VALUES (?, ?)", [(group_id, item) for item in model_ids])
            digest = _digest(settings.public_api_key)
            key = conn.execute("SELECT id FROM client_keys WHERE key_digest=?", (digest,)).fetchone()
            if key is None:
                key = conn.execute("SELECT id FROM client_keys WHERE name='legacy-public'").fetchone()
            if key is None:
                cur = conn.execute("INSERT INTO client_keys(name,key_prefix,key_digest,max_concurrency,token_limit_total,money_limit_microyuan_total) VALUES ('legacy-public', ?, ?, ?, NULL, NULL)", (settings.public_api_key[:16], digest, settings.global_concurrency_limit))
                key_id = int(cur.lastrowid)
            else:
                key_id = int(key["id"])
                conn.execute("UPDATE client_keys SET key_prefix=?, key_digest=?, max_concurrency=?, enabled=1, disabled_reason=NULL WHERE id=?", (settings.public_api_key[:16], digest, settings.global_concurrency_limit, key_id))
            conn.execute("INSERT OR IGNORE INTO client_key_groups(key_id,group_id) VALUES (?, ?)", (key_id, group_id))
            conn.commit()

    async def initialize_legacy_data(self, settings: Any) -> None:
        await asyncio.to_thread(self.initialize_legacy_data_sync, settings)

    @staticmethod
    def _provider(row: sqlite3.Row) -> ProviderRecord:
        return ProviderRecord(int(row["id"]), row["name"], row["base_url"], bool(row["enabled"]))

    @staticmethod
    def _model(row: sqlite3.Row) -> ModelRecord:
        return ModelRecord(
            int(row["id"]), row["public_name"], int(row["provider_id"]), row["upstream_name"],
            int(row["input_price_microyuan_per_million"]) if row["input_price_microyuan_per_million"] is not None else None,
            int(row["output_price_microyuan_per_million"]) if row["output_price_microyuan_per_million"] is not None else None,
            bool(row["enabled"]),
        )

    @staticmethod
    def _key(row: sqlite3.Row, token: str | None = None) -> ClientKeyRecord:
        return ClientKeyRecord(
            int(row["id"]), row["name"], token, row["key_prefix"], bool(row["enabled"]), int(row["max_concurrency"]),
            int(row["token_limit_total"]) if row["token_limit_total"] is not None else None,
            int(row["money_limit_microyuan_total"]) if row["money_limit_microyuan_total"] is not None else None,
            int(row["token_used"]), int(row["money_used_microyuan"]), row["disabled_reason"],
        )

    async def create_provider(self, name: str, base_url: str, api_key: str) -> ProviderRecord:
        return await asyncio.to_thread(self._create_provider_sync, name, base_url, api_key)

    def _create_provider_sync(self, name: str, base_url: str, api_key: str) -> ProviderRecord:
        if not name.strip() or not base_url.strip() or not api_key.strip():
            raise ValueError("provider fields are required")
        encrypted = self._cipher.encrypt(api_key.strip().encode("utf-8")).decode("ascii")
        with self._lock, self._connect() as conn:
            cursor = conn.execute("INSERT INTO providers (name, base_url, encrypted_api_key) VALUES (?, ?, ?)", (name.strip(), base_url.strip().rstrip("/"), encrypted))
            row = conn.execute("SELECT * FROM providers WHERE id = ?", (cursor.lastrowid,)).fetchone()
        return self._provider(row)

    async def list_providers(self) -> list[ProviderRecord]:
        return await asyncio.to_thread(self._list_providers_sync)

    def _list_providers_sync(self) -> list[ProviderRecord]:
        with self._lock, self._connect() as conn:
            rows = conn.execute("SELECT * FROM providers ORDER BY id").fetchall()
        return [self._provider(row) for row in rows]

    async def update_provider(self, provider_id: int, *, name: str | None = None, base_url: str | None = None, api_key: str | None = None, enabled: bool | None = None) -> ProviderRecord:
        return await asyncio.to_thread(self._update_provider_sync, int(provider_id), name, base_url, api_key, enabled)

    def _update_provider_sync(self, provider_id: int, name: str | None, base_url: str | None, api_key: str | None, enabled: bool | None) -> ProviderRecord:
        fields: list[str] = []
        values: list[Any] = []
        if name is not None:
            if not name.strip(): raise ValueError("provider name is required")
            fields.append("name=?"); values.append(name.strip())
        if base_url is not None:
            if not base_url.strip(): raise ValueError("provider base_url is required")
            fields.append("base_url=?"); values.append(base_url.strip().rstrip("/"))
        if api_key is not None:
            if not api_key.strip(): raise ValueError("provider api_key is required")
            fields.append("encrypted_api_key=?"); values.append(self._cipher.encrypt(api_key.strip().encode()).decode("ascii"))
        if enabled is not None:
            fields.append("enabled=?"); values.append(int(enabled))
        with self._lock, self._connect() as conn:
            if conn.execute("SELECT 1 FROM providers WHERE id=?", (provider_id,)).fetchone() is None:
                raise KeyError("provider not found")
            if fields:
                values.append(provider_id)
                conn.execute(f"UPDATE providers SET {', '.join(fields)}, updated_at=CURRENT_TIMESTAMP WHERE id=?", values)
            row = conn.execute("SELECT * FROM providers WHERE id=?", (provider_id,)).fetchone()
        return self._provider(row)

    async def provider_credentials(self, provider_id: int) -> tuple[ProviderRecord, str]:
        return await asyncio.to_thread(self._provider_credentials_sync, int(provider_id))

    def _provider_credentials_sync(self, provider_id: int) -> tuple[ProviderRecord, str]:
        with self._lock, self._connect() as conn:
            row = conn.execute("SELECT * FROM providers WHERE id = ?", (provider_id,)).fetchone()
        if row is None:
            raise KeyError("provider not found")
        try:
            key = self._cipher.decrypt(row["encrypted_api_key"].encode("ascii")).decode("utf-8")
        except (InvalidToken, UnicodeDecodeError, ValueError):
            raise RuntimeError("provider credential cannot be decrypted") from None
        return self._provider(row), key

    async def raw_provider_row(self, provider_id: int) -> dict[str, Any]:
        return await asyncio.to_thread(self._raw_provider_row_sync, int(provider_id))

    def _raw_provider_row_sync(self, provider_id: int) -> dict[str, Any]:
        with self._lock, self._connect() as conn:
            row = conn.execute("SELECT * FROM providers WHERE id = ?", (provider_id,)).fetchone()
        if row is None:
            raise KeyError("provider not found")
        return dict(row)

    async def create_model(self, public_name: str, provider_id: int, upstream_name: str, input_price: int | None, output_price: int | None, enabled: bool) -> ModelRecord:
        return await asyncio.to_thread(self._create_model_sync, public_name, provider_id, upstream_name, input_price, output_price, enabled)

    def _create_model_sync(self, public_name: str, provider_id: int, upstream_name: str, input_price: int | None, output_price: int | None, enabled: bool) -> ModelRecord:
        if not public_name.strip() or not upstream_name.strip():
            raise ValueError("model names are required")
        if input_price is not None and input_price < 0 or output_price is not None and output_price < 0:
            raise ValueError("model prices must not be negative")
        with self._lock, self._connect() as conn:
            if conn.execute("SELECT 1 FROM providers WHERE id = ?", (provider_id,)).fetchone() is None:
                raise ValueError("provider not found")
            cursor = conn.execute(
                "INSERT INTO models (public_name, provider_id, upstream_name, input_price_microyuan_per_million, output_price_microyuan_per_million, enabled) VALUES (?, ?, ?, ?, ?, ?)",
                (public_name.strip(), provider_id, upstream_name.strip(), input_price, output_price, int(enabled)),
            )
            row = conn.execute("SELECT * FROM models WHERE id = ?", (cursor.lastrowid,)).fetchone()
        return self._model(row)

    async def update_model(self, model_id: int, *, public_name: str | None = None, upstream_name: str | None = None, input_price: int | None = None, output_price: int | None = None, enabled: bool | None = None) -> ModelRecord:
        return await asyncio.to_thread(self._update_model_sync, int(model_id), public_name, upstream_name, input_price, output_price, enabled)

    def _update_model_sync(self, model_id: int, public_name: str | None, upstream_name: str | None, input_price: int | None, output_price: int | None, enabled: bool | None) -> ModelRecord:
        if input_price is not None and input_price < 0 or output_price is not None and output_price < 0:
            raise ValueError("model prices must not be negative")
        fields: list[str] = []
        values: list[Any] = []
        for column, value in (("public_name", public_name), ("upstream_name", upstream_name)):
            if value is not None:
                if not value.strip(): raise ValueError("model names are required")
                fields.append(f"{column}=?"); values.append(value.strip())
        for column, value in (("input_price_microyuan_per_million", input_price), ("output_price_microyuan_per_million", output_price)):
            if value is not None:
                fields.append(f"{column}=?"); values.append(value)
        if enabled is not None:
            fields.append("enabled=?"); values.append(int(enabled))
        with self._lock, self._connect() as conn:
            if conn.execute("SELECT 1 FROM models WHERE id=?", (model_id,)).fetchone() is None:
                raise KeyError("model not found")
            if fields:
                values.append(model_id)
                conn.execute(f"UPDATE models SET {', '.join(fields)}, updated_at=CURRENT_TIMESTAMP WHERE id=?", values)
            row = conn.execute("SELECT * FROM models WHERE id=?", (model_id,)).fetchone()
        return self._model(row)

    async def create_model_group(self, name: str) -> ModelGroupRecord:
        def create() -> ModelGroupRecord:
            if not name.strip():
                raise ValueError("group name is required")
            with self._lock, self._connect() as conn:
                cursor = conn.execute("INSERT INTO model_groups (name) VALUES (?)", (name.strip(),))
                return ModelGroupRecord(int(cursor.lastrowid), name.strip())
        return await asyncio.to_thread(create)

    async def set_group_models(self, group_id: int, model_ids: list[int]) -> None:
        await asyncio.to_thread(self._set_group_models_sync, int(group_id), [int(value) for value in model_ids])

    def _set_group_models_sync(self, group_id: int, model_ids: list[int]) -> None:
        with self._lock, self._connect() as conn:
            if conn.execute("SELECT 1 FROM model_groups WHERE id = ?", (group_id,)).fetchone() is None:
                raise ValueError("group not found")
            unique_ids = list(dict.fromkeys(model_ids))
            if unique_ids:
                placeholders = ",".join("?" for _ in unique_ids)
                rows = conn.execute(f"SELECT id, enabled, input_price_microyuan_per_million, output_price_microyuan_per_million FROM models WHERE id IN ({placeholders})", unique_ids).fetchall()
                if len(rows) != len(unique_ids) or any(not row["enabled"] or row["input_price_microyuan_per_million"] is None or row["output_price_microyuan_per_million"] is None for row in rows):
                    raise ValueError("model must be priced and enabled")
            conn.execute("DELETE FROM model_group_members WHERE group_id = ?", (group_id,))
            conn.executemany("INSERT INTO model_group_members (group_id, model_id) VALUES (?, ?)", [(group_id, model_id) for model_id in unique_ids])
            conn.commit()

    async def create_client_key(self, name: str, max_concurrency: int, token_limit_total: int | None, money_limit_microyuan_total: int | None) -> ClientKeyRecord:
        return await asyncio.to_thread(self._create_client_key_sync, name, max_concurrency, token_limit_total, money_limit_microyuan_total)

    def _create_client_key_sync(self, name: str, max_concurrency: int, token_limit_total: int | None, money_limit_microyuan_total: int | None) -> ClientKeyRecord:
        if not name.strip() or max_concurrency < 1:
            raise ValueError("key name and positive concurrency are required")
        if token_limit_total is not None and token_limit_total < 0 or money_limit_microyuan_total is not None and money_limit_microyuan_total < 0:
            raise ValueError("key limits must not be negative")
        token = f"crk_{uuid.uuid4().hex}_{secrets.token_urlsafe(32)}"
        prefix = token[:16]
        with self._lock, self._connect() as conn:
            cursor = conn.execute(
                "INSERT INTO client_keys (name, key_prefix, key_digest, max_concurrency, token_limit_total, money_limit_microyuan_total) VALUES (?, ?, ?, ?, ?, ?)",
                (name.strip(), prefix, _digest(token), max_concurrency, token_limit_total, money_limit_microyuan_total),
            )
            row = conn.execute("SELECT * FROM client_keys WHERE id = ?", (cursor.lastrowid,)).fetchone()
        return self._key(row, token)

    async def list_client_keys(self) -> list[dict[str, Any]]:
        return await asyncio.to_thread(self._list_client_keys_sync)

    def _list_client_keys_sync(self) -> list[dict[str, Any]]:
        with self._lock, self._connect() as conn:
            rows = conn.execute("SELECT * FROM client_keys ORDER BY id").fetchall()
        return [{**self._key(row).__dict__, "token": None} for row in rows]

    async def get_client_key(self, key_id: int) -> ClientKeyRecord:
        return await asyncio.to_thread(self._get_client_key_sync, int(key_id))

    def _get_client_key_sync(self, key_id: int) -> ClientKeyRecord:
        with self._lock, self._connect() as conn:
            row = conn.execute("SELECT * FROM client_keys WHERE id = ?", (key_id,)).fetchone()
        if row is None:
            raise KeyError("client key not found")
        return self._key(row)

    async def set_key_groups(self, key_id: int, group_ids: list[int]) -> None:
        await asyncio.to_thread(self._set_key_groups_sync, int(key_id), [int(value) for value in group_ids])

    def _set_key_groups_sync(self, key_id: int, group_ids: list[int]) -> None:
        with self._lock, self._connect() as conn:
            if conn.execute("SELECT 1 FROM client_keys WHERE id = ?", (key_id,)).fetchone() is None:
                raise ValueError("client key not found")
            unique_ids = list(dict.fromkeys(group_ids))
            if unique_ids:
                placeholders = ",".join("?" for _ in unique_ids)
                count = conn.execute(f"SELECT COUNT(*) FROM model_groups WHERE id IN ({placeholders})", unique_ids).fetchone()[0]
                if count != len(unique_ids):
                    raise ValueError("model group not found")
            conn.execute("DELETE FROM client_key_groups WHERE key_id = ?", (key_id,))
            conn.executemany("INSERT INTO client_key_groups (key_id, group_id) VALUES (?, ?)", [(key_id, group_id) for group_id in unique_ids])
            conn.commit()

    async def authenticate_key(self, token: str) -> ClientKeyRecord | None:
        return await asyncio.to_thread(self._authenticate_key_sync, token)

    def _authenticate_key_sync(self, token: str) -> ClientKeyRecord | None:
        if not token or len(token) > 256:
            return None
        with self._lock, self._connect() as conn:
            row = conn.execute("SELECT * FROM client_keys WHERE key_digest = ?", (_digest(token),)).fetchone()
        if row is None or not row["enabled"]:
            return None
        return self._key(row)

    async def authorized_models(self, token: str) -> list[ModelRecord]:
        return await asyncio.to_thread(self._authorized_models_sync, token)

    def _authorized_models_sync(self, token: str) -> list[ModelRecord]:
        if not token or len(token) > 256:
            return []
        with self._lock, self._connect() as conn:
            rows = conn.execute(
                "SELECT DISTINCT m.* FROM models m JOIN model_group_members gm ON gm.model_id = m.id JOIN client_key_groups kg ON kg.group_id = gm.group_id JOIN client_keys ck ON ck.id = kg.key_id JOIN providers p ON p.id = m.provider_id WHERE ck.key_digest = ? AND ck.enabled = 1 AND m.enabled = 1 AND m.input_price_microyuan_per_million IS NOT NULL AND m.output_price_microyuan_per_million IS NOT NULL AND p.enabled = 1 ORDER BY m.public_name",
                (_digest(token),),
            ).fetchall()
        return [self._model(row) for row in rows]

    async def provider_model(self, public_name: str, token: str) -> ModelRecord | None:
        models = await self.authorized_models(token)
        return next((model for model in models if model.public_name == public_name), None)

    async def list_models(self) -> list[ModelRecord]:
        return await asyncio.to_thread(self._list_models_sync)

    async def get_model(self, model_id: int) -> ModelRecord | None:
        return await asyncio.to_thread(self._get_model_sync, int(model_id))

    def _get_model_sync(self, model_id: int) -> ModelRecord | None:
        with self._lock, self._connect() as conn:
            row = conn.execute("SELECT * FROM models WHERE id=?", (model_id,)).fetchone()
        return self._model(row) if row else None

    async def sync_provider_models(self, provider_id: int, upstream_ids: list[str] | tuple[str, ...]) -> list[str]:
        return await asyncio.to_thread(self._sync_provider_models_sync, int(provider_id), list(upstream_ids))

    def _sync_provider_models_sync(self, provider_id: int, upstream_ids: list[str]) -> list[str]:
        clean = list(dict.fromkeys(item.strip() for item in upstream_ids if isinstance(item, str) and item.strip()))
        if not clean:
            raise ValueError("no usable models")
        with self._lock, self._connect() as conn:
            if conn.execute("SELECT 1 FROM providers WHERE id=?", (provider_id,)).fetchone() is None:
                raise KeyError("provider not found")
            imported: list[str] = []
            provider_name = conn.execute("SELECT name FROM providers WHERE id=?", (provider_id,)).fetchone()[0]
            for upstream_name in clean:
                public_name = f"{provider_name}/{upstream_name}"
                row = conn.execute("SELECT id FROM models WHERE provider_id=? AND upstream_name=?", (provider_id, upstream_name)).fetchone()
                if row is None:
                    conn.execute("INSERT OR IGNORE INTO models (public_name, provider_id, upstream_name, enabled) VALUES (?, ?, ?, 0)", (public_name, provider_id, upstream_name))
                imported.append(public_name)
            conn.commit()
        return imported

    def _list_models_sync(self) -> list[ModelRecord]:
        with self._lock, self._connect() as conn:
            rows = conn.execute("SELECT * FROM models ORDER BY public_name").fetchall()
        return [self._model(row) for row in rows]

    async def list_model_groups(self) -> list[ModelGroupRecord]:
        return await asyncio.to_thread(self._list_model_groups_sync)

    def _list_model_groups_sync(self) -> list[ModelGroupRecord]:
        with self._lock, self._connect() as conn:
            rows = conn.execute("SELECT id, name FROM model_groups ORDER BY name").fetchall()
        return [ModelGroupRecord(int(row["id"]), row["name"]) for row in rows]

    async def group_models(self, group_id: int) -> list[ModelRecord]:
        return await asyncio.to_thread(self._group_models_sync, int(group_id))

    def _group_models_sync(self, group_id: int) -> list[ModelRecord]:
        with self._lock, self._connect() as conn:
            rows = conn.execute(
                "SELECT m.* FROM models m JOIN model_group_members gm ON gm.model_id=m.id WHERE gm.group_id=? ORDER BY m.public_name",
                (group_id,),
            ).fetchall()
        return [self._model(row) for row in rows]

    async def key_groups(self, key_id: int) -> list[ModelGroupRecord]:
        return await asyncio.to_thread(self._key_groups_sync, int(key_id))

    def _key_groups_sync(self, key_id: int) -> list[ModelGroupRecord]:
        with self._lock, self._connect() as conn:
            rows = conn.execute(
                "SELECT g.* FROM model_groups g JOIN client_key_groups kg ON kg.group_id=g.id WHERE kg.key_id=? ORDER BY g.name",
                (key_id,),
            ).fetchall()
        return [ModelGroupRecord(int(row["id"]), row["name"]) for row in rows]

    async def set_client_key_enabled(self, key_id: int, enabled: bool, reason: str | None = None) -> ClientKeyRecord:
        return await asyncio.to_thread(self._set_client_key_enabled_sync, int(key_id), bool(enabled), reason)

    def _set_client_key_enabled_sync(self, key_id: int, enabled: bool, reason: str | None) -> ClientKeyRecord:
        with self._lock, self._connect() as conn:
            if conn.execute("SELECT 1 FROM client_keys WHERE id=?", (key_id,)).fetchone() is None:
                raise KeyError("client key not found")
            conn.execute("UPDATE client_keys SET enabled=?, disabled_reason=?, updated_at=CURRENT_TIMESTAMP WHERE id=?", (int(enabled), reason if not enabled else None, key_id))
            row = conn.execute("SELECT * FROM client_keys WHERE id=?", (key_id,)).fetchone()
        return self._key(row)

    async def update_client_key(self, key_id: int, *, max_concurrency: int | None = None, token_limit_total: int | None = None, money_limit_microyuan_total: int | None = None) -> ClientKeyRecord:
        return await asyncio.to_thread(self._update_client_key_sync, int(key_id), max_concurrency, token_limit_total, money_limit_microyuan_total)

    def _update_client_key_sync(self, key_id: int, max_concurrency: int | None, token_limit_total: int | None, money_limit_microyuan_total: int | None) -> ClientKeyRecord:
        if max_concurrency is not None and max_concurrency < 1:
            raise ValueError("max_concurrency must be positive")
        if token_limit_total is not None and token_limit_total < 0 or money_limit_microyuan_total is not None and money_limit_microyuan_total < 0:
            raise ValueError("key limits must not be negative")
        changes: list[str] = []
        values: list[Any] = []
        for column, value in (("max_concurrency", max_concurrency), ("token_limit_total", token_limit_total), ("money_limit_microyuan_total", money_limit_microyuan_total)):
            if value is not None:
                changes.append(f"{column}=?"); values.append(value)
        if changes:
            values.append(key_id)
            with self._lock, self._connect() as conn:
                conn.execute(f"UPDATE client_keys SET {', '.join(changes)}, updated_at=CURRENT_TIMESTAMP WHERE id=?", values)
                row = conn.execute("SELECT * FROM client_keys WHERE id=?", (key_id,)).fetchone()
        else:
            row = self._get_client_key_sync(key_id)
            return row
        if row is None:
            raise KeyError("client key not found")
        return self._key(row)

    async def reserve_for_key(
        self, key_id: int, model_id: int, input_tokens: int, output_tokens: int,
        reserved_tokens: int, reserved_money_microyuan: int,
    ) -> Reservation:
        return await asyncio.to_thread(self._reserve_for_key_sync, int(key_id), int(model_id), int(input_tokens), int(output_tokens), int(reserved_tokens), int(reserved_money_microyuan))

    def _reserve_for_key_sync(self, key_id: int, model_id: int, input_tokens: int, output_tokens: int, reserved_tokens: int, reserved_money: int) -> Reservation:
        if input_tokens < 0 or output_tokens < 0 or reserved_tokens <= 0 or reserved_money < 0:
            raise ValueError("invalid reservation values")
        started_dt = datetime.now(timezone.utc)
        started = started_dt.isoformat(timespec="microseconds").replace("+00:00", "Z")
        billing_date = started_dt.date().isoformat()
        with self._lock, self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            key = conn.execute("SELECT * FROM client_keys WHERE id=?", (key_id,)).fetchone()
            model = conn.execute("SELECT m.*, p.enabled AS provider_enabled FROM models m JOIN providers p ON p.id=m.provider_id WHERE m.id=?", (model_id,)).fetchone()
            if key is None or not key["enabled"]:
                raise QuotaExceeded("key_disabled")
            if model is None or not model["enabled"] or not model["provider_enabled"] or model["input_price_microyuan_per_million"] is None or model["output_price_microyuan_per_million"] is None:
                raise QuotaExceeded("model_unavailable")
            allowed = conn.execute("SELECT 1 FROM client_key_groups kg JOIN model_group_members gm ON gm.group_id=kg.group_id WHERE kg.key_id=? AND gm.model_id=?", (key_id, model_id)).fetchone()
            if allowed is None:
                raise QuotaExceeded("model_not_allowed")
            reserved = conn.execute("SELECT COALESCE(SUM(reserved_tokens),0), COALESCE(SUM(reserved_money_microyuan),0) FROM key_reservations WHERE client_key_id=? AND status='reserved'", (key_id,)).fetchone()
            token_total = int(key["token_used"]) + int(reserved[0]) + reserved_tokens
            money_total = int(key["money_used_microyuan"]) + int(reserved[1]) + reserved_money
            if key["token_limit_total"] is not None and token_total > int(key["token_limit_total"]):
                raise QuotaExceeded("key_token_quota_exceeded")
            if key["money_limit_microyuan_total"] is not None and money_total > int(key["money_limit_microyuan_total"]):
                raise QuotaExceeded("key_money_quota_exceeded")
            request_id = "tenant-" + uuid.uuid4().hex
            cursor = conn.execute(
                "INSERT INTO key_reservations (request_id, started_at_utc, billing_date_bj, model_id, status, reserved_tokens, input_tokens, client_key_id, reserved_money_microyuan) VALUES (?, ?, ?, ?, 'reserved', ?, ?, ?, ?)",
                (request_id, started, billing_date, model_id, reserved_tokens, input_tokens, key_id, reserved_money),
            )
            conn.commit()
        return Reservation(int(cursor.lastrowid), request_id, started, billing_date, model["public_name"], False, input_tokens, reserved_tokens, key_id, int(model["provider_id"]), reserved_money)

    async def settle_for_key(self, reservation_id: int, input_tokens: int, output_tokens: int, charged_tokens: int, charged_money_microyuan: int | None = None) -> None:
        await asyncio.to_thread(self._settle_for_key_sync, int(reservation_id), int(input_tokens), int(output_tokens), int(charged_tokens), charged_money_microyuan)

    def _settle_for_key_sync(self, reservation_id: int, input_tokens: int, output_tokens: int, charged_tokens: int, charged_money: int | None) -> None:
        if min(input_tokens, output_tokens, charged_tokens) < 0:
            raise ValueError("usage values must not be negative")
        with self._lock, self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            row = conn.execute("SELECT r.*, m.input_price_microyuan_per_million AS input_price, m.output_price_microyuan_per_million AS output_price FROM key_reservations r JOIN models m ON m.id=r.model_id WHERE r.id=?", (reservation_id,)).fetchone()
            if row is None:
                raise KeyError("reservation not found")
            if row["status"] != "reserved" or row["client_key_id"] is None:
                raise RuntimeError("reservation already settled")
            if charged_money is None:
                charged_money = math.ceil((input_tokens * int(row["input_price"] or 0) + output_tokens * int(row["output_price"] or 0)) / 1_000_000)
            key = conn.execute("SELECT * FROM client_keys WHERE id=?", (row["client_key_id"],)).fetchone()
            if key is None:
                raise KeyError("client key not found")
            status = "completed"
            conn.execute("UPDATE key_reservations SET reserved_tokens=0, charged_tokens=?, reserved_money_microyuan=0, charged_money_microyuan=?, status=?, finished_at_utc=?, input_tokens=?, output_tokens=? WHERE id=? AND status='reserved'", (charged_tokens, charged_money, status, datetime.now(timezone.utc).isoformat(timespec="microseconds").replace("+00:00", "Z"), input_tokens, output_tokens, reservation_id))
            new_tokens = int(key["token_used"]) + charged_tokens
            new_money = int(key["money_used_microyuan"]) + int(charged_money)
            exhausted = (key["token_limit_total"] is not None and new_tokens >= int(key["token_limit_total"])) or (key["money_limit_microyuan_total"] is not None and new_money >= int(key["money_limit_microyuan_total"]))
            conn.execute("UPDATE client_keys SET token_used=?, money_used_microyuan=?, enabled=CASE WHEN ? THEN 0 ELSE enabled END, disabled_reason=CASE WHEN ? THEN 'quota_exhausted' ELSE disabled_reason END, updated_at=CURRENT_TIMESTAMP WHERE id=?", (new_tokens, new_money, int(exhausted), int(exhausted), row["client_key_id"]))
            conn.commit()

    # 额度接口在下一阶段扩展；保留导入以保持异常类型统一。
    _quota_error = QuotaExceeded
