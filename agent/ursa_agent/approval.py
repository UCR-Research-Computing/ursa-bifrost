"""Approval gate for cluster actions.

The model may PLAN an action (job_submit, job_cancel, job_hold, job_release):
bifrost returns a plan with a single-use confirm_token. The model may never
CONFIRM one. A *_confirm tool call from the model is refused in code here, and
the pending plan is parked in session state for the person to approve with a
button in the chat page (or an explicit approval message over A2A). Only then
does the server call the *_confirm tool, with the token it parked, not one the
model supplied.

This is deliberately not left to the prompt: an instruction like "wait for the
user's yes" can be talked around; a refused tool call cannot.
"""

from __future__ import annotations

import json
import time
from dataclasses import asdict, dataclass, field
from typing import Any

# Tools that change things on the cluster, mapped to their confirm step.
PLAN_TOOLS = {
    "job_submit": "job_submit_confirm",
    "job_cancel": "job_cancel_confirm",
    "job_hold": "job_hold_confirm",
    "job_release": "job_release_confirm",
}
CONFIRM_TOOLS = set(PLAN_TOOLS.values())

STATE_KEY = "pending_actions"
REFUSAL = (
    "Refused: only the person can approve a cluster action. The plan is waiting for their "
    "Approve/Reject in the chat. Tell them what the plan does (partition, nodes, time, "
    "worst-case cost) and that it needs their approval; do not call any *_confirm tool."
)


@dataclass
class Pending:
    """A planned action waiting for the person's decision."""

    id: str
    tool: str  # the plan tool, e.g. job_submit
    confirm_tool: str
    token: str  # confirm_token from bifrost (never shown to the model again)
    summary: dict[str, Any]
    created: float = field(default_factory=time.time)
    expires_at: str = ""

    def public(self) -> dict[str, Any]:
        """What the UI may show: everything except the token."""
        d = asdict(self)
        d.pop("token")
        return d


def _payload(result: Any) -> dict[str, Any] | None:
    """Pull bifrost's JSON envelope out of an MCP tool result (dict or CallToolResult)."""
    if result is None:
        return None
    if isinstance(result, dict):
        if "data" in result and isinstance(result["data"], dict):
            return result["data"]
        # ADK wraps MCP results; the text content holds bifrost's envelope
        content = result.get("content")
        if isinstance(content, list) and content:
            text = content[0].get("text") if isinstance(content[0], dict) else None
            if text:
                try:
                    env = json.loads(text)
                except ValueError:
                    return None
                return env.get("data") if isinstance(env, dict) else None
        sc = result.get("structuredContent") or result.get("structured_content")
        if isinstance(sc, dict):
            return sc.get("data", sc)
        return None
    content = getattr(result, "content", None)
    if content:
        text = getattr(content[0], "text", None)
        if text:
            try:
                env = json.loads(text)
            except ValueError:
                return None
            return env.get("data") if isinstance(env, dict) else None
    return None


def summarize(tool: str, data: dict[str, Any]) -> dict[str, Any]:
    """The fields a person needs to decide (no token)."""
    keys = [
        "action",
        "job_id",
        "job_name",
        "partition",
        "nodes",
        "time_limit",
        "worst_case_usd",
        "committed_today_usd",
        "day_cap_usd",
        "scheduler_test",
        "warnings",
        "effect",
        "state",
        "remote_dir",
        "expires_at",
    ]
    out = {k: data[k] for k in keys if k in data}
    out.setdefault("action", tool.removeprefix("job_"))
    return out


def before_tool(tool, args: dict[str, Any], tool_context) -> dict[str, Any] | None:
    """ADK before_tool_callback: refuse any confirm call from the model."""
    if tool.name in CONFIRM_TOOLS:
        return {"error": REFUSAL}
    return None


def after_tool(tool, args: dict[str, Any], tool_context, tool_response: Any) -> dict[str, Any] | None:
    """ADK after_tool_callback: park plan tokens; hide them from the model."""
    if tool.name not in PLAN_TOOLS:
        return None
    data = _payload(tool_response)
    if not data or not data.get("confirm_token"):
        return None  # refused by bifrost (caps, bad id): let the model see the error as is
    pend = Pending(
        id=f"a{int(time.time() * 1000) % 10**10}",
        tool=tool.name,
        confirm_tool=PLAN_TOOLS[tool.name],
        token=data["confirm_token"],
        summary=summarize(tool.name, data),
        expires_at=data.get("expires_at", ""),
    )
    pending = dict(tool_context.state.get(STATE_KEY) or {})
    pending[pend.id] = asdict(pend)
    tool_context.state[STATE_KEY] = pending
    shown = dict(pend.summary)
    shown["approval"] = (
        f"Waiting for the person to approve or reject (approval id {pend.id}). "
        "Nothing has happened on the cluster yet."
    )
    return {"plan": shown}


def take(state: dict[str, Any], action_id: str) -> Pending | None:
    """Remove and return a pending action (single use)."""
    pending = dict(state.get(STATE_KEY) or {})
    raw = pending.pop(action_id, None)
    if raw is None:
        return None
    state[STATE_KEY] = pending
    return Pending(**raw)


def list_pending(state: dict[str, Any]) -> list[dict[str, Any]]:
    return [Pending(**v).public() for v in (state.get(STATE_KEY) or {}).values()]
