"""Dashboard panels: read-only bifrost tool calls behind an allow-list, with a
per-person stale-while-revalidate cache (SPEC section 20).

Nothing here can write: each panel names exactly one read tool, and its
arguments are built from validated query values, never passed through.
"""

from __future__ import annotations

import asyncio
import re
import time
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

# Tools the dashboard may call. Anything that changes state (submit, cancel,
# hold, release, *_confirm, upload_prepare, results_link, file reads) is absent
# on purpose; tests/test_panels.py checks this list against WRITE_TOOLS.
READ_TOOLS = frozenset(
    {
        "cluster_status",
        "partitions",
        "jobs_list",
        "job_show",
        "job_explain",
        "my_usage",
        "waste_report",
        "storage_usage",
        "uploads_list",
        "interactive_help",
        "modules_search",
        # R2 (staff); bifrost registers these only for R2 users
        "health",
        "jobs_list_all",
        "usage_report",
        "waste_report_all",
    }
)
WRITE_TOOLS = frozenset(
    {
        "job_submit",
        "job_submit_confirm",
        "job_cancel",
        "job_cancel_confirm",
        "job_hold",
        "job_hold_confirm",
        "job_release",
        "job_release_confirm",
        "upload_prepare",
        "results_link",
        "files_read",
        "files_list",
        "job_results",
        "job_log_tail",
        "ticket_draft",
        "env_check",
    }
)

JOB_ID = re.compile(r"^\d{1,10}(_\d{1,7})?$")
NAME = re.compile(r"^[A-Za-z0-9_.+-]{1,64}$")  # partition / user / module-ish
TIME = re.compile(r"^(\d{1,4}|\d{1,2}:\d{2}(:\d{2})?|\d{1,2}-\d{1,2}(:\d{2}){0,2})$")
RANGES = {"7d": "now-7days", "30d": "now-30days", "1d": "now-1days"}


class BadArgs(ValueError):
    """A panel argument failed validation (HTTP 400)."""


def _range(q: dict[str, str], default: str) -> str:
    v = q.get("range", default)
    if v not in RANGES:
        raise BadArgs(f"range must be one of {', '.join(RANGES)}")
    return RANGES[v]


def _job(q: dict[str, str]) -> str:
    v = q.get("job", "")
    if not JOB_ID.match(v):
        raise BadArgs("job must be a Slurm job id such as 315 or 260_3")
    return v


def _int(q: dict[str, str], key: str, lo: int, hi: int, default: int | None = None) -> int | None:
    v = q.get(key, "")
    if v == "":
        return default
    if not v.isdigit() or not lo <= int(v) <= hi:
        raise BadArgs(f"{key} must be a whole number from {lo} to {hi}")
    return int(v)


def _name(q: dict[str, str], key: str, required: bool = False) -> str:
    v = q.get(key, "")
    if not v:
        if required:
            raise BadArgs(f"{key} is required")
        return ""
    if not NAME.match(v):
        raise BadArgs(f"{key} has characters that are not allowed")
    return v


def _interactive(q: dict[str, str]) -> dict[str, Any]:
    a: dict[str, Any] = {}
    if p := _name(q, "partition"):
        a["partition"] = p
    for k, hi in (("nodes", 4), ("cpus", 64), ("gpus", 8)):
        if (n := _int(q, k, 1, hi)) is not None:
            a[k] = n
    t = q.get("time", "")
    if t:
        if not TIME.match(t):
            raise BadArgs("time must be minutes or H:MM[:SS] or D-HH[:MM]")
        a["time"] = t
    return a


def _query(q: dict[str, str]) -> dict[str, Any]:
    v = q.get("q", "").strip()
    if len(v) > 64 or any(c in v for c in "\n\r\t;|&$`<>"):
        raise BadArgs("search text must be at most 64 plain characters")
    return {"query": v}


@dataclass(frozen=True)
class Panel:
    tool: str
    ttl: int  # seconds before a background refresh
    args: Callable[[dict[str, str]], dict[str, Any]] = field(default=lambda q: {})
    staff: bool = False
    keyed: tuple[str, ...] = ()  # query keys that make a separate cache entry


PANELS: dict[str, Panel] = {
    "pulse": Panel("cluster_status", 60),
    "partitions": Panel("partitions", 3600),
    "jobs": Panel("jobs_list", 60, lambda q: {"since": _range(q, "7d"), "limit": 50}, keyed=("range",)),
    "job": Panel("job_show", 60, lambda q: {"job_id": _job(q)}, keyed=("job",)),
    "explain": Panel("job_explain", 300, lambda q: {"job_id": _job(q)}, keyed=("job",)),
    "usage": Panel(
        "my_usage",
        600,
        lambda q: {"since": _range(q, "30d"), "group_by": _choice(q, "by", ("partition", "state"))},
        keyed=("range", "by"),
    ),
    "waste": Panel("waste_report", 600, lambda q: {"since": _range(q, "7d")}, keyed=("range",)),
    "storage": Panel("storage_usage", 1800),
    "uploads": Panel("uploads_list", 120),
    "interactive": Panel(
        "interactive_help", 3600, _interactive, keyed=("partition", "nodes", "cpus", "gpus", "time")
    ),
    "modules": Panel("modules_search", 3600, _query, keyed=("q",)),
    # staff (R2)
    "health": Panel("health", 120, staff=True),
    "alljobs": Panel(
        "jobs_list_all",
        120,
        lambda q: {"since": _range(q, "1d"), "limit": 100} | ({"user": u} if (u := _name(q, "user")) else {}),
        staff=True,
        keyed=("range", "user"),
    ),
    "usersusage": Panel(
        "usage_report",
        600,
        lambda q: {"since": _range(q, "30d"), "group_by": "user"},
        staff=True,
        keyed=("range",),
    ),
    "allwaste": Panel(
        "waste_report_all", 600, lambda q: {"since": _range(q, "7d")}, staff=True, keyed=("range",)
    ),
}


def _choice(q: dict[str, str], key: str, allowed: tuple[str, ...]) -> str:
    v = q.get(key, allowed[0])
    if v not in allowed:
        raise BadArgs(f"{key} must be one of {', '.join(allowed)}")
    return v


def resolve(name: str, query: dict[str, str]) -> tuple[Panel, dict[str, Any], str]:
    """Panel, validated tool arguments, and the cache key for this request."""
    p = PANELS.get(name)
    if p is None:
        raise KeyError(name)
    if p.tool not in READ_TOOLS or p.tool in WRITE_TOOLS:  # belt and braces
        raise PermissionError(f"{p.tool} is not a read tool")
    args = p.args(query)
    key = name + "?" + "&".join(f"{k}={query.get(k, '')}" for k in p.keyed)
    return p, args, key


Caller = Callable[[str, dict[str, Any]], Awaitable[dict[str, Any]]]


@dataclass
class Entry:
    value: dict[str, Any] | None = None
    fetched: float = 0.0
    task: asyncio.Task | None = None


class PanelCache:
    """Per-person panel results: answer from cache, refresh in the background.

    One in-flight bifrost call per key; at most `parallel` calls per person,
    so the dashboard never takes more than half of bifrost's 8 SSH sessions.
    """

    def __init__(self, parallel: int = 4, now: Callable[[], float] = time.monotonic):
        self.entries: dict[str, Entry] = {}
        self.sem = asyncio.Semaphore(parallel)
        self.now = now

    async def _fetch(self, e: Entry, call: Caller, tool: str, args: dict[str, Any]) -> dict[str, Any]:
        async with self.sem:
            out = await call(tool, args)
        # cache only successful answers; an error is shown once and retried next time
        if not out.get("is_error"):
            e.value, e.fetched = out, self.now()
        return out

    async def get(
        self, key: str, ttl: int, call: Caller, tool: str, args: dict[str, Any], force: bool = False
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        """(result, meta). meta: cached (bool), age_s, refreshing (bool)."""
        e = self.entries.setdefault(key, Entry())
        fresh = e.value is not None and self.now() - e.fetched < ttl
        if e.value is not None and not force:
            refreshing = False
            if not fresh and (e.task is None or e.task.done()):
                e.task = asyncio.create_task(self._fetch(e, call, tool, args))
                refreshing = True
            elif e.task is not None and not e.task.done():
                refreshing = True
            return e.value, {"cached": True, "age_s": round(self.now() - e.fetched), "refreshing": refreshing}
        if e.task is None or e.task.done():
            e.task = asyncio.create_task(self._fetch(e, call, tool, args))
        out = await asyncio.shield(e.task)
        return out, {"cached": False, "age_s": 0, "refreshing": False}

    def clear(self) -> None:
        for e in self.entries.values():
            if e.task is not None and not e.task.done():
                e.task.cancel()
        self.entries.clear()
