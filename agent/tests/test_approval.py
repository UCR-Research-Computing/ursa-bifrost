"""The approval gate: the model can plan, never confirm; plan tokens are hidden from it."""

import json
from types import SimpleNamespace

from ursa_agent import approval


class Ctx:
    def __init__(self):
        self.state = {}


def tool(name):
    return SimpleNamespace(name=name)


def plan_response(token="bf1-secret-token", **extra):
    data = {
        "confirm_token": token,
        "partition": "computehigh",
        "nodes": 1,
        "time_limit": "0 h 05 min",
        "worst_case_usd": 0.16,
        "job_name": "t",
        "expires_at": "2026-10-02T02:00:00Z",
        **extra,
    }
    return {"content": [{"type": "text", "text": json.dumps({"as_of": "x", "data": data})}], "isError": False}


def test_confirm_tools_are_refused_for_the_model():
    for name in approval.CONFIRM_TOOLS:
        out = approval.before_tool(tool(name), {"confirm_token": "anything"}, Ctx())
        assert out and "Refused" in out["error"], name


def test_read_tools_and_plan_tools_pass_through():
    for name in ["cluster_status", "jobs_list", "job_submit", "job_cancel", "job_hold", "job_release"]:
        assert approval.before_tool(tool(name), {}, Ctx()) is None


def test_plan_token_is_parked_and_hidden_from_model():
    ctx = Ctx()
    shown = approval.after_tool(tool("job_submit"), {"script": "x"}, ctx, plan_response())
    assert "bf1-secret-token" not in json.dumps(shown)
    assert shown["plan"]["worst_case_usd"] == 0.16
    pend = approval.list_pending(ctx.state)
    assert len(pend) == 1 and "token" not in pend[0]
    assert pend[0]["confirm_tool"] == "job_submit_confirm"


def test_take_is_single_use():
    ctx = Ctx()
    approval.after_tool(tool("job_cancel"), {"job_id": "9"}, ctx, plan_response(token="bf1-t2", job_id="9"))
    aid = approval.list_pending(ctx.state)[0]["id"]
    p = approval.take(ctx.state, aid)
    assert p.token == "bf1-t2" and p.confirm_tool == "job_cancel_confirm"
    assert approval.take(ctx.state, aid) is None
    assert approval.list_pending(ctx.state) == []


def test_bifrost_refusal_is_passed_through_untouched():
    ctx = Ctx()
    refused = {
        "content": [{"type": "text", "text": "over cap: 8 nodes requested, cap is 4"}],
        "isError": True,
    }
    assert approval.after_tool(tool("job_submit"), {}, ctx, refused) is None
    assert approval.list_pending(ctx.state) == []


def test_non_plan_tools_untouched():
    ctx = Ctx()
    assert approval.after_tool(tool("jobs_list"), {}, ctx, plan_response()) is None
    assert approval.list_pending(ctx.state) == []
