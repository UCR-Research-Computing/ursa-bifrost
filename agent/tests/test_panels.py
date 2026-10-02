"""Dashboard panels (SPEC section 20): read-only allow-list, argument checks, cache."""

import asyncio
import re
from pathlib import Path

import pytest

from ursa_agent import panels


# ---------------------------------------------------------------- read-only by construction
def test_every_panel_calls_a_read_tool_only():
    for name, p in panels.PANELS.items():
        assert p.tool in panels.READ_TOOLS, name
        assert p.tool not in panels.WRITE_TOOLS, name
        assert not p.tool.endswith("_confirm"), name


def test_read_and_write_lists_do_not_overlap():
    assert not panels.READ_TOOLS & panels.WRITE_TOOLS
    for t in panels.READ_TOOLS:
        assert not re.search(r"submit|cancel|hold|release|confirm|upload_prepare|results_link|files_", t), t


def test_resolve_refuses_a_panel_whose_tool_is_not_a_read_tool(monkeypatch):
    monkeypatch.setitem(panels.PANELS, "evil", panels.Panel("job_cancel_confirm", 60))
    with pytest.raises(PermissionError):
        panels.resolve("evil", {})


def test_unknown_panel_is_a_key_error():
    with pytest.raises(KeyError):
        panels.resolve("job_submit", {})


def test_staff_panels_are_the_r2_tools():
    staff = {p.tool for p in panels.PANELS.values() if p.staff}
    assert staff == {"health", "jobs_list_all", "usage_report", "waste_report_all"}


# ---------------------------------------------------------------- argument validation
@pytest.mark.parametrize("bad", ["1;ls", "", "abc", "12 34", "1" * 11, "315_", "315_12345678", "$(id)"])
def test_job_ids_are_validated(bad):
    with pytest.raises(panels.BadArgs):
        panels.resolve("job", {"job": bad})
    with pytest.raises(panels.BadArgs):
        panels.resolve("explain", {"job": bad})


def test_good_job_id_passes_and_is_keyed():
    p, args, key = panels.resolve("job", {"job": "260_3"})
    assert p.tool == "job_show" and args == {"job_id": "260_3"} and key == "job?job=260_3"


def test_ranges_come_from_a_fixed_list():
    _, args, _ = panels.resolve("jobs", {"range": "30d"})
    assert args == {"since": "now-30days", "limit": 50}
    for bad in ["now-999days", "2020-01-01", "7d; rm", "x"]:
        with pytest.raises(panels.BadArgs):
            panels.resolve("jobs", {"range": bad})


def test_usage_group_by_is_limited():
    _, args, _ = panels.resolve("usage", {"by": "state"})
    assert args["group_by"] == "state"
    for bad in ["user", "job", "partition;"]:  # user is the staff report, not my_usage
        with pytest.raises(panels.BadArgs):
            panels.resolve("usage", {"by": bad})


def test_interactive_args_are_checked_and_passed_typed():
    _, args, _ = panels.resolve("interactive", {"partition": "computehigh", "cpus": "4", "time": "1:00:00"})
    assert args == {"partition": "computehigh", "cpus": 4, "time": "1:00:00"}
    for q in [
        {"cpus": "0"},
        {"cpus": "999"},
        {"cpus": "4x"},
        {"partition": "a b"},
        {"time": "1h; ls"},
        {"nodes": "9"},
    ]:
        with pytest.raises(panels.BadArgs):
            panels.resolve("interactive", q)


def test_module_query_is_plain_text():
    _, args, _ = panels.resolve("modules", {"q": " gromacs "})
    assert args == {"query": "gromacs"}
    for bad in ["a;b", "x" * 65, "$(id)", "a|b", "a\nb"]:
        with pytest.raises(panels.BadArgs):
            panels.resolve("modules", {"q": bad})


def test_staff_user_filter_is_checked():
    _, args, _ = panels.resolve("alljobs", {"user": "someone_ucr_edu"})
    assert args["user"] == "someone_ucr_edu"
    with pytest.raises(panels.BadArgs):
        panels.resolve("alljobs", {"user": "x;y"})


def test_unkeyed_query_values_do_not_reach_the_tool():
    # extra query parameters are ignored: they cannot add tool arguments
    _, args, key = panels.resolve("pulse", {"job_id": "1", "confirm_token": "t"})
    assert args == {} and key == "pulse?"


# ---------------------------------------------------------------- cache
class Clock:
    def __init__(self):
        self.t = 1000.0

    def __call__(self):
        return self.t


def ok(n):
    return {"is_error": False, "result": {"data": n}}


async def test_cache_serves_stale_and_refreshes_in_background():
    clock, calls = Clock(), []

    async def call(tool, args):
        calls.append(tool)
        await asyncio.sleep(0)
        return ok(len(calls))

    c = panels.PanelCache(now=clock)
    out, meta = await c.get("k", 60, call, "cluster_status", {})
    assert out == ok(1) and meta["cached"] is False
    out, meta = await c.get("k", 60, call, "cluster_status", {})
    assert out == ok(1) and meta["cached"] is True and meta["refreshing"] is False and len(calls) == 1
    clock.t += 61  # stale: answer at once with the old value, refresh behind
    out, meta = await c.get("k", 60, call, "cluster_status", {})
    assert out == ok(1) and meta["refreshing"] is True
    await c.entries["k"].task
    out, _ = await c.get("k", 60, call, "cluster_status", {})
    assert out == ok(2) and len(calls) == 2


async def test_concurrent_first_loads_share_one_call():
    calls = []
    gate = asyncio.Event()

    async def call(tool, args):
        calls.append(tool)
        await gate.wait()
        return ok(1)

    c = panels.PanelCache()
    t = [asyncio.create_task(c.get("k", 60, call, "jobs_list", {})) for _ in range(5)]
    await asyncio.sleep(0.01)
    gate.set()
    res = await asyncio.gather(*t)
    assert len(calls) == 1 and all(r[0] == ok(1) for r in res)


async def test_errors_are_not_cached():
    n = {"i": 0}

    async def call(tool, args):
        n["i"] += 1
        return {"is_error": True, "result": "boom"} if n["i"] == 1 else ok(2)

    c = panels.PanelCache()
    out, _ = await c.get("k", 60, call, "jobs_list", {})
    assert out["is_error"]
    out, meta = await c.get("k", 60, call, "jobs_list", {})
    assert out == ok(2) and meta["cached"] is False


async def test_parallel_calls_are_capped_per_person():
    live = {"now": 0, "max": 0}

    async def call(tool, args):
        live["now"] += 1
        live["max"] = max(live["max"], live["now"])
        await asyncio.sleep(0.01)
        live["now"] -= 1
        return ok(1)

    c = panels.PanelCache(parallel=4)
    await asyncio.gather(*[c.get(f"k{i}", 60, call, "jobs_list", {}) for i in range(12)])
    assert live["max"] == 4


async def test_force_refetches_and_clear_drops_everything():
    calls = []

    async def call(tool, args):
        calls.append(1)
        return ok(len(calls))

    c = panels.PanelCache()
    await c.get("k", 3600, call, "partitions", {})
    out, meta = await c.get("k", 3600, call, "partitions", {}, force=True)
    assert out == ok(2) and meta["cached"] is False
    c.clear()
    assert c.entries == {}


# ---------------------------------------------------------------- page safety (static files)
STATIC = Path(__file__).resolve().parents[1] / "ursa_agent" / "static"


def test_page_has_no_inline_script_or_style():
    html = (STATIC / "index.html").read_text()
    assert not re.search(r"<script(?![^>]*\bsrc=)[^>]*>", html), "inline <script>"
    assert "<style" not in html and " style=" not in html and "onclick=" not in html


def test_js_never_writes_html_from_data():
    js = (STATIC / "app.js").read_text()
    code = re.sub(r"/\*.*?\*/", "", js, flags=re.DOTALL)  # block comments first
    code = "\n".join(line for line in code.splitlines() if not line.strip().startswith("//"))
    for bad in ("innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"):
        assert bad not in code, bad


def test_waste_renderers_handle_node_items_without_job_or_user():
    """idle-node waste items have no job_id and no user (live, 2026-10-02): the page
    showed 'job undefined' and a user called 'undefined'."""
    js = (STATIC / "app.js").read_text()
    assert "'  job ' + it.job_id" not in js
    assert "by[it.user] = " not in js
    assert "it.job_id ? 'job ' + it.job_id" in js and "it.user || " in js
