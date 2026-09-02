from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone

import pytest

from db import Ledger, QuotaExceeded, Reservation


UTC = timezone.utc


@pytest.fixture
async def db(tmp_path):
    ledger = Ledger(tmp_path / "relay.db", retention_days=7)
    yield ledger
    await ledger.close()


def stamp(value: datetime) -> str:
    return value.astimezone(UTC).isoformat().replace("+00:00", "Z")


@pytest.mark.asyncio
async def test_second_reservation_is_rejected_when_prior_reservation_uses_quota(db):
    now = datetime(2026, 8, 31, 4, 0, tzinfo=UTC)
    await db.reserve(
        "a", stamp(now), "2026-08-31", "m", False, 600, 400,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    with pytest.raises(QuotaExceeded) as error:
        await db.reserve(
            "b", stamp(now), "2026-08-31", "m", False, 600, 700,
            rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
        )
    assert error.value.code == "token_quota_exceeded"


@pytest.mark.asyncio
async def test_rpm_is_counted_inside_the_same_transaction(db):
    now = datetime(2026, 8, 31, 4, 0, tzinfo=UTC)
    await db.reserve(
        "a", stamp(now), "2026-08-31", "m", False, 1, 1,
        rpm_limit=1, token_limit_5h=1000, token_limit_daily=1000,
    )
    with pytest.raises(QuotaExceeded) as error:
        await db.reserve(
            "b", stamp(now + timedelta(seconds=30)), "2026-08-31", "m", False, 1, 1,
            rpm_limit=1, token_limit_5h=1000, token_limit_daily=1000,
        )
    assert error.value.code == "rpm_exceeded"


@pytest.mark.asyncio
async def test_cancelled_reservation_releases_all_quota(db):
    now = datetime(2026, 8, 31, 4, 0, tzinfo=UTC)
    record = await db.reserve(
        "a", stamp(now), "2026-08-31", "m", False, 600, 900,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    await db.cancel(record.id, stamp(now + timedelta(seconds=1)))
    assert await db.occupied_tokens(stamp(now), "2026-08-31") == (0, 0)
    row = await db.get(record.id)
    assert row["reserved_tokens"] == 0
    assert row["charged_tokens"] == 0
    assert row["status"] == "rejected"


@pytest.mark.asyncio
async def test_settlement_clears_reservation_and_keeps_actual_charge(db):
    now = datetime(2026, 8, 31, 4, 0, tzinfo=UTC)
    record = await db.reserve(
        "a", stamp(now), "2026-08-31", "m", False, 500, 1000,
        rpm_limit=10, token_limit_5h=2000, token_limit_daily=2000,
    )
    await db.settle(record.id, stamp(now + timedelta(seconds=8)), "completed", 200, 80, 280, 200, 8)
    row = await db.get(record.id)
    assert row["reserved_tokens"] == 0
    assert row["charged_tokens"] == 280
    assert row["status"] == "completed"
    assert row["input_tokens"] == 200
    assert row["output_tokens"] == 80


@pytest.mark.asyncio
async def test_rolling_window_and_beijing_billing_date_are_independent(db):
    # 21:00 UTC is 05:00 Beijing on the next local billing date.
    now = datetime(2026, 8, 31, 21, 0, tzinfo=UTC)
    await db.reserve(
        "old", stamp(now - timedelta(hours=5, seconds=1)), "2026-08-31", "m", False, 1, 800,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    await db.reserve(
        "today", stamp(now), "2026-09-01", "m", False, 1, 900,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    with pytest.raises(QuotaExceeded) as error:
        await db.reserve(
            "daily", stamp(now + timedelta(seconds=1)), "2026-09-01", "m", False, 1, 200,
            rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
        )
    assert error.value.code == "token_quota_exceeded"
    assert await db.occupied_tokens(stamp(now), "2026-09-01") == (900, 900)


@pytest.mark.asyncio
async def test_database_uses_wal_and_prunes_only_old_records(db, tmp_path):
    now = datetime(2026, 8, 31, 4, 0, tzinfo=UTC)
    old = await db.reserve(
        "old", stamp(now - timedelta(days=8)), "2026-08-23", "m", False, 1, 1,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    recent = await db.reserve(
        "recent", stamp(now), "2026-08-31", "m", False, 1, 1,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    await db.prune(now_utc=stamp(now))
    assert await db.get(old.id) is None
    assert await db.get(recent.id) is not None

    import sqlite3

    with sqlite3.connect(tmp_path / "relay.db") as conn:
        assert conn.execute("PRAGMA journal_mode").fetchone()[0].lower() == "wal"
        assert conn.execute("PRAGMA busy_timeout").fetchone()[0] == 5000


@pytest.mark.asyncio
async def test_competing_ledgers_allow_only_one_final_quota_reservation(tmp_path):
    path = tmp_path / "race.db"
    first = Ledger(path)
    second = Ledger(path)
    args = dict(
        started_at_utc="2026-08-31T04:00:00Z",
        billing_date_bj="2026-08-31",
        model="m",
        stream=False,
        input_tokens=1,
        reservation_tokens=600,
        rpm_limit=10,
        token_limit_5h=1000,
        token_limit_daily=1000,
    )
    results = await asyncio.gather(
        first.reserve("race-a", **args),
        second.reserve("race-b", **args),
        return_exceptions=True,
    )
    assert sum(isinstance(result, Reservation) for result in results) == 1
    assert sum(isinstance(result, QuotaExceeded) for result in results) == 1


@pytest.mark.asyncio
async def test_future_records_do_not_count_against_current_windows(tmp_path):
    ledger = Ledger(tmp_path / "future.db")
    await ledger.reserve(
        "future", "2026-08-31T05:00:00Z", "2026-09-01", "m", False, 1, 900,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    current = await ledger.reserve(
        "current", "2026-08-31T04:00:00Z", "2026-08-31", "m", False, 1, 900,
        rpm_limit=10, token_limit_5h=1000, token_limit_daily=1000,
    )
    assert current.request_id == "current"


@pytest.mark.asyncio
async def test_monitoring_snapshot_and_recent_rows_are_redacted(db):
    now = datetime(2026, 9, 1, 4, 0, tzinfo=UTC)
    completed = await db.reserve(
        "completed-id", stamp(now - timedelta(minutes=2)), "2026-09-01", "m", False, 3, 8,
        rpm_limit=30, token_limit_5h=1000, token_limit_daily=1000,
    )
    await db.settle(completed.id, stamp(now - timedelta(minutes=1)), "completed", 3, 2, 5, 200, 100)
    rejected = await db.reserve(
        "rejected-id", stamp(now), "2026-09-01", "m", True, 3, 8,
        rpm_limit=30, token_limit_5h=1000, token_limit_daily=1000,
    )
    await db.cancel(rejected.id, stamp(now), 429, 1)

    snapshot = await db.monitoring_snapshot(stamp(now), "2026-09-01", recent_limit=50)

    assert snapshot["rpm_count"] == 1
    assert snapshot["five_hour_tokens"] == 5
    assert snapshot["daily_tokens"] == 5
    assert snapshot["last_hour"] == {
        "completed": 1,
        "failed": 0,
        "aborted": 0,
        "rejected": 1,
        "reserved": 0,
        "error_rate": 0.0,
    }
    assert len(snapshot["trend"]) == 12
    assert snapshot["recent"][0] == {
        "request_id": "rejected-id",
        "started_at_utc": now.isoformat(timespec="microseconds").replace("+00:00", "Z"),
        "model": "m",
        "stream": True,
        "status": "rejected",
        "http_status": 429,
        "duration_ms": 1,
        "charged_tokens": 0,
    }
    assert set(snapshot["recent"][0]) == {
        "request_id", "started_at_utc", "model", "stream", "status", "http_status", "duration_ms", "charged_tokens"
    }
