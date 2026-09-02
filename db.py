"""Durable SQLite request ledger and atomic quota reservations."""

from __future__ import annotations

import asyncio
import sqlite3
import threading
from dataclasses import dataclass
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any


UTC = timezone.utc


class QuotaExceeded(RuntimeError):
    """Raised when an RPM or token quota cannot accommodate a request."""

    def __init__(self, code: str, message: str | None = None):
        self.code = code
        super().__init__(message or code)


@dataclass(frozen=True)
class Reservation:
    id: int
    request_id: str
    started_at_utc: str
    billing_date_bj: str
    model: str
    stream: bool
    input_tokens: int
    reserved_tokens: int
    client_key_id: int | None = None
    provider_id: int | None = None
    reserved_money_microyuan: int = 0


def _iso_utc(value: datetime | str) -> str:
    if isinstance(value, datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=UTC)
        value = value.astimezone(UTC)
        return value.isoformat(timespec="microseconds").replace("+00:00", "Z")
    if not isinstance(value, str) or not value.strip():
        raise ValueError("timestamp must be a UTC datetime or ISO string")
    text = value.strip()
    # Normalize common ISO forms so SQLite's text ordering remains chronological.
    try:
        parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
    except ValueError:
        raise ValueError("timestamp must be ISO formatted") from None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=UTC)
    return parsed.astimezone(UTC).isoformat(timespec="microseconds").replace("+00:00", "Z")


class Ledger:
    """A small SQLite-backed ledger.

    Public methods are asynchronous and run SQLite work in a worker thread,
    allowing FastAPI's event loop to continue serving other requests. A process
    lock keeps each operation's transaction isolated while SQLite's five-second
    busy timeout remains in force for external readers.
    """

    def __init__(self, db_path: str | Path, retention_days: int = 7):
        self.db_path = Path(db_path)
        self.retention_days = int(retention_days)
        if self.retention_days <= 0:
            raise ValueError("retention_days must be positive")
        self.db_path.parent.mkdir(parents=True, exist_ok=True)
        self._lock = threading.Lock()
        self._initialize()

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(str(self.db_path), timeout=5.0)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA busy_timeout=5000")
        conn.execute("PRAGMA synchronous=NORMAL")
        return conn

    def _initialize(self) -> None:
        with self._lock, self._connect() as conn:
            conn.execute("PRAGMA journal_mode=WAL")
            conn.execute("PRAGMA synchronous=NORMAL")
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS requests (
                    id INTEGER PRIMARY KEY AUTOINCREMENT,
                    request_id TEXT NOT NULL,
                    started_at_utc TEXT NOT NULL,
                    finished_at_utc TEXT,
                    billing_date_bj TEXT NOT NULL,
                    model TEXT NOT NULL,
                    stream INTEGER NOT NULL,
                    status TEXT NOT NULL,
                    reserved_tokens INTEGER NOT NULL DEFAULT 0,
                    input_tokens INTEGER,
                    output_tokens INTEGER,
                    charged_tokens INTEGER NOT NULL DEFAULT 0,
                    client_key_id INTEGER,
                    provider_id INTEGER,
                    reserved_money_microyuan INTEGER NOT NULL DEFAULT 0,
                    charged_money_microyuan INTEGER NOT NULL DEFAULT 0,
                    http_status INTEGER,
                    duration_ms INTEGER
                );
                CREATE INDEX IF NOT EXISTS idx_requests_started_at
                    ON requests (started_at_utc);
                CREATE INDEX IF NOT EXISTS idx_requests_billing_date
                    ON requests (billing_date_bj);
                CREATE INDEX IF NOT EXISTS idx_requests_request_id
                    ON requests (request_id);
                """
            )
            # Idempotent migration for databases created by older releases.
            columns = {row["name"] for row in conn.execute("PRAGMA table_info(requests)").fetchall()}
            for name, declaration in (
                ("client_key_id", "INTEGER"),
                ("provider_id", "INTEGER"),
                ("reserved_money_microyuan", "INTEGER NOT NULL DEFAULT 0"),
                ("charged_money_microyuan", "INTEGER NOT NULL DEFAULT 0"),
            ):
                if name not in columns:
                    conn.execute(f"ALTER TABLE requests ADD COLUMN {name} {declaration}")
            conn.execute("CREATE INDEX IF NOT EXISTS idx_requests_client_key ON requests (client_key_id)")

    async def close(self) -> None:
        # Connections are intentionally short-lived; this method exists for a
        # predictable application shutdown hook and API symmetry.
        await asyncio.sleep(0)

    async def healthcheck(self) -> bool:
        return await asyncio.to_thread(self._healthcheck_sync)

    def _healthcheck_sync(self) -> bool:
        with self._lock, self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            conn.execute("SELECT 1").fetchone()
            conn.rollback()
        return True

    async def reserve(
        self,
        request_id: str,
        started_at_utc: datetime | str,
        billing_date_bj: str,
        model: str,
        stream: bool,
        input_tokens: int,
        reservation_tokens: int,
        rpm_limit: int,
        token_limit_5h: int,
        token_limit_daily: int,
    ) -> Reservation:
        """Atomically enforce all quotas and insert a reserved request."""
        started = _iso_utc(started_at_utc)
        if not request_id or not billing_date_bj or not model:
            raise ValueError("request metadata is required")
        if input_tokens < 0 or reservation_tokens <= 0:
            raise ValueError("token values are invalid")
        for name, value in (
            ("rpm_limit", rpm_limit),
            ("token_limit_5h", token_limit_5h),
            ("token_limit_daily", token_limit_daily),
        ):
            if value <= 0:
                raise ValueError(f"{name} must be positive")

        return await asyncio.to_thread(
            self._reserve_sync,
            request_id,
            started,
            str(billing_date_bj),
            model,
            bool(stream),
            int(input_tokens),
            int(reservation_tokens),
            int(rpm_limit),
            int(token_limit_5h),
            int(token_limit_daily),
        )

    def _reserve_sync(
        self,
        request_id: str,
        started: str,
        billing_date_bj: str,
        model: str,
        stream: bool,
        input_tokens: int,
        reservation_tokens: int,
        rpm_limit: int,
        five_hour_limit: int,
        daily_limit: int,
    ) -> Reservation:
        now = datetime.fromisoformat(started.replace("Z", "+00:00"))
        window_start = (now - timedelta(hours=5)).isoformat(timespec="microseconds").replace("+00:00", "Z")
        rpm_start = (now - timedelta(seconds=60)).isoformat(timespec="microseconds").replace("+00:00", "Z")
        with self._lock, self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            rpm_count = conn.execute(
                "SELECT COUNT(*) FROM requests WHERE started_at_utc >= ? AND started_at_utc <= ?",
                (rpm_start, started),
            ).fetchone()[0]
            if rpm_count >= rpm_limit:
                raise QuotaExceeded("rpm_exceeded")
            five_hour = conn.execute(
                "SELECT COALESCE(SUM(charged_tokens + reserved_tokens), 0) "
                "FROM requests WHERE started_at_utc >= ? AND started_at_utc <= ?",
                (window_start, started),
            ).fetchone()[0]
            if five_hour + reservation_tokens > five_hour_limit:
                raise QuotaExceeded("token_quota_exceeded")
            daily = conn.execute(
                "SELECT COALESCE(SUM(charged_tokens + reserved_tokens), 0) "
                "FROM requests WHERE billing_date_bj = ? AND started_at_utc <= ?",
                (billing_date_bj, started),
            ).fetchone()[0]
            if daily + reservation_tokens > daily_limit:
                raise QuotaExceeded("token_quota_exceeded")
            cursor = conn.execute(
                "INSERT INTO requests "
                "(request_id, started_at_utc, billing_date_bj, model, stream, status, "
                "reserved_tokens, input_tokens) VALUES (?, ?, ?, ?, ?, 'reserved', ?, ?)",
                (request_id, started, billing_date_bj, model, int(stream), reservation_tokens, input_tokens),
            )
            row_id = int(cursor.lastrowid)
            conn.commit()
        return Reservation(row_id, request_id, started, billing_date_bj, model, stream, input_tokens, reservation_tokens)

    async def cancel(
        self,
        reservation_id: int,
        finished_at_utc: datetime | str,
        http_status: int = 429,
        duration_ms: int | None = None,
    ) -> None:
        finished = _iso_utc(finished_at_utc)
        await asyncio.to_thread(self._cancel_sync, int(reservation_id), finished, int(http_status), duration_ms)

    def _cancel_sync(self, reservation_id: int, finished: str, http_status: int, duration_ms: int | None) -> None:
        with self._lock, self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            conn.execute(
                "UPDATE requests SET reserved_tokens = 0, charged_tokens = 0, status = 'rejected', "
                "finished_at_utc = ?, http_status = ?, duration_ms = ? WHERE id = ? AND status = 'reserved'",
                (finished, http_status, duration_ms, reservation_id),
            )
            conn.commit()

    async def settle(
        self,
        reservation_id: int,
        finished_at_utc: datetime | str,
        status: str,
        input_tokens: int | None,
        output_tokens: int | None,
        charged_tokens: int,
        http_status: int | None,
        duration_ms: int | None,
    ) -> None:
        finished = _iso_utc(finished_at_utc)
        if charged_tokens < 0:
            raise ValueError("charged_tokens must not be negative")
        await asyncio.to_thread(
            self._settle_sync,
            int(reservation_id), finished, str(status), input_tokens, output_tokens,
            int(charged_tokens), http_status, duration_ms,
        )

    def _settle_sync(
        self,
        reservation_id: int,
        finished: str,
        status: str,
        input_tokens: int | None,
        output_tokens: int | None,
        charged_tokens: int,
        http_status: int | None,
        duration_ms: int | None,
    ) -> None:
        with self._lock, self._connect() as conn:
            conn.execute("BEGIN IMMEDIATE")
            conn.execute(
                "UPDATE requests SET reserved_tokens = 0, charged_tokens = ?, status = ?, "
                "finished_at_utc = ?, input_tokens = ?, output_tokens = ?, http_status = ?, duration_ms = ? "
                "WHERE id = ? AND status = 'reserved'",
                (charged_tokens, status, finished, input_tokens, output_tokens, http_status, duration_ms, reservation_id),
            )
            conn.commit()

    async def get(self, reservation_id: int) -> dict[str, Any] | None:
        return await asyncio.to_thread(self._get_sync, int(reservation_id))

    def _get_sync(self, reservation_id: int) -> dict[str, Any] | None:
        with self._lock, self._connect() as conn:
            row = conn.execute("SELECT * FROM requests WHERE id = ?", (reservation_id,)).fetchone()
        return dict(row) if row else None

    async def latest(self) -> dict[str, Any] | None:
        return await asyncio.to_thread(self._latest_sync)

    def _latest_sync(self) -> dict[str, Any] | None:
        with self._lock, self._connect() as conn:
            row = conn.execute("SELECT * FROM requests ORDER BY id DESC LIMIT 1").fetchone()
        return dict(row) if row else None

    async def occupied_tokens(self, now_utc: datetime | str, billing_date_bj: str) -> tuple[int, int]:
        now = _iso_utc(now_utc)
        return await asyncio.to_thread(self._occupied_sync, now, str(billing_date_bj))

    def _occupied_sync(self, now: str, billing_date_bj: str) -> tuple[int, int]:
        current = datetime.fromisoformat(now.replace("Z", "+00:00"))
        window_start = (current - timedelta(hours=5)).isoformat(timespec="microseconds").replace("+00:00", "Z")
        with self._lock, self._connect() as conn:
            five_hour = conn.execute(
                "SELECT COALESCE(SUM(charged_tokens + reserved_tokens), 0) "
                "FROM requests WHERE started_at_utc >= ? AND started_at_utc <= ?",
                (window_start, now),
            ).fetchone()[0]
            daily = conn.execute(
                "SELECT COALESCE(SUM(charged_tokens + reserved_tokens), 0) "
                "FROM requests WHERE billing_date_bj = ? AND started_at_utc <= ?",
                (billing_date_bj, now),
            ).fetchone()[0]
        return int(five_hour), int(daily)

    async def recent_requests(self, limit: int = 50) -> list[dict[str, Any]]:
        bounded_limit = max(1, min(int(limit), 50))
        return await asyncio.to_thread(self._recent_requests_sync, bounded_limit)

    def _recent_requests_sync(self, limit: int) -> list[dict[str, Any]]:
        with self._lock, self._connect() as conn:
            rows = conn.execute(
                "SELECT request_id, started_at_utc, model, stream, status, http_status, duration_ms, charged_tokens "
                "FROM requests ORDER BY id DESC LIMIT ?",
                (limit,),
            ).fetchall()
        return [
            {
                "request_id": row["request_id"],
                "started_at_utc": row["started_at_utc"],
                "model": row["model"],
                "stream": bool(row["stream"]),
                "status": row["status"],
                "http_status": row["http_status"],
                "duration_ms": row["duration_ms"],
                "charged_tokens": int(row["charged_tokens"]),
            }
            for row in rows
        ]

    async def monitoring_snapshot(
        self,
        now_utc: datetime | str,
        billing_date_bj: str,
        recent_limit: int = 10,
    ) -> dict[str, Any]:
        now = _iso_utc(now_utc)
        bounded_limit = max(1, min(int(recent_limit), 50))
        return await asyncio.to_thread(self._monitoring_snapshot_sync, now, str(billing_date_bj), bounded_limit)

    def _monitoring_snapshot_sync(self, now: str, billing_date_bj: str, recent_limit: int) -> dict[str, Any]:
        current = datetime.fromisoformat(now.replace("Z", "+00:00"))
        rpm_start = (current - timedelta(seconds=60)).isoformat(timespec="microseconds").replace("+00:00", "Z")
        five_hour_start = (current - timedelta(hours=5)).isoformat(timespec="microseconds").replace("+00:00", "Z")
        hour_start_dt = current - timedelta(hours=1)
        hour_start = hour_start_dt.isoformat(timespec="microseconds").replace("+00:00", "Z")
        with self._lock, self._connect() as conn:
            conn.execute("BEGIN")
            rpm_count = conn.execute(
                "SELECT COUNT(*) FROM requests WHERE started_at_utc >= ? AND started_at_utc <= ?",
                (rpm_start, now),
            ).fetchone()[0]
            five_hour_tokens = conn.execute(
                "SELECT COALESCE(SUM(charged_tokens + reserved_tokens), 0) FROM requests "
                "WHERE started_at_utc >= ? AND started_at_utc <= ?",
                (five_hour_start, now),
            ).fetchone()[0]
            daily_tokens = conn.execute(
                "SELECT COALESCE(SUM(charged_tokens + reserved_tokens), 0) FROM requests "
                "WHERE billing_date_bj = ? AND started_at_utc <= ?",
                (billing_date_bj, now),
            ).fetchone()[0]
            outcome_rows = conn.execute(
                "SELECT status, COUNT(*) AS count FROM requests WHERE started_at_utc >= ? AND started_at_utc <= ? "
                "GROUP BY status",
                (hour_start, now),
            ).fetchall()
            trend_rows = conn.execute(
                "SELECT started_at_utc FROM requests WHERE started_at_utc >= ? AND started_at_utc <= ? "
                "ORDER BY started_at_utc",
                (hour_start, now),
            ).fetchall()
            recent_rows = conn.execute(
                "SELECT request_id, started_at_utc, model, stream, status, http_status, duration_ms, charged_tokens "
                "FROM requests ORDER BY id DESC LIMIT ?",
                (recent_limit,),
            ).fetchall()
            conn.rollback()

        outcomes = {"completed": 0, "failed": 0, "aborted": 0, "rejected": 0, "reserved": 0}
        for row in outcome_rows:
            if row["status"] in outcomes:
                outcomes[row["status"]] = int(row["count"])
        denominator = outcomes["completed"] + outcomes["failed"]
        outcomes["error_rate"] = outcomes["failed"] / denominator if denominator else 0.0

        buckets = [0] * 12
        for row in trend_rows:
            started = datetime.fromisoformat(row["started_at_utc"].replace("Z", "+00:00"))
            offset = int((started - hour_start_dt).total_seconds() // 300)
            buckets[max(0, min(offset, len(buckets) - 1))] += 1
        trend = [
            {
                "started_at_utc": (hour_start_dt + timedelta(minutes=index * 5)).isoformat(timespec="seconds").replace("+00:00", "Z"),
                "count": count,
            }
            for index, count in enumerate(buckets)
        ]
        recent = [
            {
                "request_id": row["request_id"],
                "started_at_utc": row["started_at_utc"],
                "model": row["model"],
                "stream": bool(row["stream"]),
                "status": row["status"],
                "http_status": row["http_status"],
                "duration_ms": row["duration_ms"],
                "charged_tokens": int(row["charged_tokens"]),
            }
            for row in recent_rows
        ]
        return {
            "rpm_count": int(rpm_count),
            "five_hour_tokens": int(five_hour_tokens),
            "daily_tokens": int(daily_tokens),
            "last_hour": outcomes,
            "trend": trend,
            "recent": recent,
        }

    async def prune(self, now_utc: datetime | str | None = None) -> int:
        now = _iso_utc(now_utc or datetime.now(UTC))
        return await asyncio.to_thread(self._prune_sync, now)

    def _prune_sync(self, now: str) -> int:
        current = datetime.fromisoformat(now.replace("Z", "+00:00"))
        cutoff = (current - timedelta(days=self.retention_days)).isoformat(timespec="microseconds").replace("+00:00", "Z")
        with self._lock, self._connect() as conn:
            cursor = conn.execute("DELETE FROM requests WHERE started_at_utc < ?", (cutoff,))
            return cursor.rowcount
