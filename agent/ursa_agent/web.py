"""ursa-agent web service: chat page, sign-in through bifrost, approvals, A2A.

Sign-in: the agent is a regular OAuth client of bifrost (dynamic client
registration + PKCE), exactly like Hermes. The person signs in with Google on
bifrost's page; the agent gets a bifrost access token for that person and
uses it on every MCP call. The agent never sees Google tokens and has no
cluster access of its own.

Sessions live in memory (Cloud Run min-instances 0: a cold start means signing
in again). The browser holds only a signed session-id cookie.
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import json
import logging
import os
import secrets
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import httpx
from google.adk.runners import Runner
from google.adk.sessions import InMemorySessionService
from google.genai import types as genai_types
from itsdangerous import BadSignature, URLSafeTimedSerializer
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse, RedirectResponse, Response
from starlette.routing import Mount, Route
from starlette.staticfiles import StaticFiles

from . import __version__, approval, panels
from .agent import TOKEN_KEY, build_agent

log = logging.getLogger("ursa_agent")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s %(message)s")

APP = "ursa_agent"
BASE_URL = os.environ.get("AGENT_BASE_URL", "http://localhost:8080").rstrip("/")
BIFROST = os.environ.get("BIFROST_MCP_URL", "https://bifrost-mcp-125853442225.us-central1.run.app/mcp")
BIFROST_BASE = BIFROST.removesuffix("/mcp")
COOKIE = "ursa_sid"
# bifrost's error prefix when it cannot log in to the cluster (backend.ErrUnreachable)
UNREACHABLE = "cannot reach the cluster"
CONNECTING = "Connecting to the cluster. This can take a minute after a quiet spell; retrying."

SECURE = BASE_URL.startswith("https://")
signer = URLSafeTimedSerializer(os.environ["AGENT_SESSION_SECRET"], salt="ursa-agent-sid")

# web sessions: sid -> {email, tiers, token, refresh, exp, adk_session, client_id, cache, lock}
WEB: dict[str, dict[str, Any]] = {}
PENDING_LOGIN: dict[str, dict[str, Any]] = {}  # state -> {verifier, sid, created}
CLIENT: dict[str, str] = {}  # bifrost OAuth client registration (memory; re-registered on cold start)

sessions = InMemorySessionService()
agent = build_agent()
runner = Runner(app_name=APP, agent=agent, session_service=sessions)


# ---------------------------------------------------------------- bifrost OAuth client


async def _client_id(http: httpx.AsyncClient) -> str:
    if CLIENT.get("id"):
        return CLIENT["id"]
    r = await http.post(
        f"{BIFROST_BASE}/register",
        json={
            "client_name": "Ursa Major assistant (ursa-agent)",
            "redirect_uris": [f"{BASE_URL}/auth/callback"],
            "token_endpoint_auth_method": "none",
        },
    )
    r.raise_for_status()
    CLIENT["id"] = r.json()["client_id"]
    return CLIENT["id"]


def _s256(v: str) -> str:
    return base64.urlsafe_b64encode(hashlib.sha256(v.encode()).digest()).rstrip(b"=").decode()


def _sid(request: Request) -> str | None:
    raw = request.cookies.get(COOKIE)
    if not raw:
        return None
    try:
        return signer.loads(raw, max_age=12 * 3600)
    except BadSignature:
        return None


def _web(request: Request) -> dict[str, Any] | None:
    sid = _sid(request)
    return WEB.get(sid) if sid else None


async def _fresh_token(w: dict[str, Any]) -> str:
    """The person's bifrost access token, refreshed when near expiry.

    bifrost rotates refresh tokens (each one works once), and the dashboard
    makes parallel calls, so the refresh runs under a per-session lock: the
    first caller refreshes, the others wait and reuse the new token.
    """
    if w["exp"] - time.time() > 120:
        return w["token"]
    lock = w.setdefault("lock", asyncio.Lock())
    async with lock:
        if w["exp"] - time.time() > 120:  # refreshed while we waited
            return w["token"]
        async with httpx.AsyncClient(timeout=30) as http:
            r = await http.post(
                f"{BIFROST_BASE}/token",
                data={
                    "grant_type": "refresh_token",
                    "refresh_token": w["refresh"],
                    "client_id": w["client_id"],
                },
            )
        if r.status_code != 200:
            raise PermissionError("sign-in expired")
        t = r.json()
        w.update(token=t["access_token"], refresh=t["refresh_token"], exp=time.time() + t["expires_in"])
        await _set_state(w, {TOKEN_KEY: w["token"]})
        return w["token"]


async def _set_state(w: dict[str, Any], delta: dict[str, Any]) -> None:
    """Write into the ADK session state (token, cleared approvals)."""
    from google.adk.events import Event, EventActions

    s = await sessions.get_session(app_name=APP, user_id=w["email"], session_id=w["adk_session"])
    await sessions.append_event(s, Event(author="system", actions=EventActions(state_delta=delta)))


async def login(request: Request) -> Response:
    async with httpx.AsyncClient(timeout=30) as http:
        cid = await _client_id(http)
    state, verifier = secrets.token_urlsafe(24), secrets.token_urlsafe(48)
    sid = secrets.token_urlsafe(24)
    PENDING_LOGIN[state] = {"verifier": verifier, "sid": sid, "created": time.time()}
    q = httpx.QueryParams(
        response_type="code",
        client_id=cid,
        redirect_uri=f"{BASE_URL}/auth/callback",
        state=state,
        code_challenge=_s256(verifier),
        code_challenge_method="S256",
        resource=BIFROST,
        scope="bifrost",
    )
    return RedirectResponse(f"{BIFROST_BASE}/authorize?{q}", status_code=302)


async def callback(request: Request) -> Response:
    p = PENDING_LOGIN.pop(request.query_params.get("state", ""), None)
    if not p or time.time() - p["created"] > 600:
        return HTMLResponse("Sign-in expired. <a href='/login'>Start again</a>.", status_code=400)
    if err := request.query_params.get("error"):
        desc = request.query_params.get("error_description", "")
        return HTMLResponse(f"Sign-in refused: {_esc(err)} {_esc(desc)}", status_code=403)
    async with httpx.AsyncClient(timeout=30) as http:
        cid = await _client_id(http)
        r = await http.post(
            f"{BIFROST_BASE}/token",
            data={
                "grant_type": "authorization_code",
                "code": request.query_params.get("code", ""),
                "client_id": cid,
                "redirect_uri": f"{BASE_URL}/auth/callback",
                "code_verifier": p["verifier"],
            },
        )
        if r.status_code != 200:
            return HTMLResponse(
                "Sign-in failed at the token step. <a href='/login'>Try again</a>.", status_code=400
            )
        tok = r.json()
        email, tiers = await _whoami_full(tok["access_token"])
    s = await sessions.create_session(app_name=APP, user_id=email, state={TOKEN_KEY: tok["access_token"]})
    WEB[p["sid"]] = {
        "email": email,
        "tiers": tiers,
        "token": tok["access_token"],
        "refresh": tok["refresh_token"],
        "exp": time.time() + tok["expires_in"],
        "adk_session": s.id,
        "client_id": cid,
    }
    log.info("signin %s", email)
    resp = RedirectResponse("/", status_code=302)
    resp.set_cookie(
        COOKIE, signer.dumps(p["sid"]), httponly=True, secure=SECURE, samesite="lax", max_age=12 * 3600
    )
    return resp


async def _whoami_full(token: str) -> tuple[str, list[str]]:
    """Ask bifrost who this token belongs to and its tiers (GET /whoami, bifrost v0.5.5+)."""
    async with httpx.AsyncClient(timeout=30) as http:
        r = await http.get(f"{BIFROST_BASE}/whoami", headers={"Authorization": f"Bearer {token}"})
    if r.status_code == 200:
        j = r.json()
        return j["email"], [str(t) for t in j.get("tiers") or []]
    raise PermissionError("bifrost did not accept the new token")


async def _whoami(token: str) -> str:
    return (await _whoami_full(token))[0]


async def logout(request: Request) -> Response:
    sid = _sid(request)
    w = WEB.pop(sid, None) if sid else None
    if w:
        if w.get("cache"):
            w["cache"].clear()
        async with httpx.AsyncClient(timeout=10) as http:
            await http.post(f"{BIFROST_BASE}/revoke", data={"token": w["refresh"]})
            await http.post(f"{BIFROST_BASE}/revoke", data={"token": w["token"]})
    resp = RedirectResponse("/", status_code=302)
    resp.delete_cookie(COOKIE)
    return resp


# ---------------------------------------------------------------- chat + approvals


def _events_text(events: list) -> tuple[str, list[str]]:
    texts, tools = [], []
    for ev in events:
        for fc in ev.get_function_calls() or []:
            tools.append(fc.name)
        if ev.content and ev.content.parts and ev.author != "user":
            for part in ev.content.parts:
                if getattr(part, "text", None) and not getattr(part, "thought", False):
                    texts.append(part.text)
    return "\n".join(t for t in texts if t.strip()), tools


async def _run(w: dict[str, Any], text: str) -> dict[str, Any]:
    await _fresh_token(w)
    msg = genai_types.Content(role="user", parts=[genai_types.Part(text=text)])
    events = [
        ev async for ev in runner.run_async(user_id=w["email"], session_id=w["adk_session"], new_message=msg)
    ]
    reply, tools = _events_text(events)
    s = await sessions.get_session(app_name=APP, user_id=w["email"], session_id=w["adk_session"])
    return {"reply": reply, "tools": tools, "pending": approval.list_pending(s.state)}


async def chat(request: Request) -> Response:
    w = _web(request)
    if not w:
        return JSONResponse({"error": "sign in first", "login": "/login"}, status_code=401)
    body = await request.json()
    text = str(body.get("message", "")).strip()[:8000]
    if not text:
        return JSONResponse({"error": "empty message"}, status_code=400)
    try:
        return JSONResponse(await _run(w, text))
    except PermissionError:
        WEB.pop(_sid(request) or "", None)
        return JSONResponse({"error": "sign-in expired", "login": "/login"}, status_code=401)


async def _confirm_with_bifrost(token: str, tool: str, confirm_token: str) -> dict[str, Any]:
    return await _call_bifrost(token, tool, {"confirm_token": confirm_token})


async def decide(request: Request) -> Response:
    """The person's Approve/Reject. Only path that calls a *_confirm tool."""
    w = _web(request)
    if not w:
        return JSONResponse({"error": "sign in first"}, status_code=401)
    body = await request.json()
    action_id, verdict = str(body.get("id", "")), body.get("decision")
    if verdict not in ("approve", "reject"):
        return JSONResponse({"error": "decision must be approve or reject"}, status_code=400)
    s = await sessions.get_session(app_name=APP, user_id=w["email"], session_id=w["adk_session"])
    state = dict(s.state)
    pend = approval.take(state, action_id)
    if not pend:
        return JSONResponse({"error": "no such pending action (already decided or expired)"}, status_code=404)
    await _set_state(w, {approval.STATE_KEY: state[approval.STATE_KEY]})
    if verdict == "reject":
        log.info(
            "reject %s %s %s",
            w["email"],
            pend.tool,
            pend.summary.get("job_id") or pend.summary.get("job_name"),
        )
        note = f"The person REJECTED the planned {pend.tool} ({json.dumps(pend.summary)}). Nothing was done."
        out = await _run(w, f"[system] {note} Acknowledge briefly.")
        out["decision"] = {"id": action_id, "status": "rejected"}
        return JSONResponse(out)
    token = await _fresh_token(w)
    res = await _confirm_with_bifrost(token, pend.confirm_tool, pend.token)
    log.info("approve %s %s -> error=%s", w["email"], pend.confirm_tool, res["is_error"])
    data = res["result"].get("data", res["result"]) if isinstance(res["result"], dict) else res["result"]
    note = (
        f"The person APPROVED the planned {pend.tool}. The system called {pend.confirm_tool}; "
        f"bifrost answered: {json.dumps(data)[:1500]}"
    )
    out = await _run(w, f"[system] {note} Tell the person the outcome and the next step in one or two lines.")
    out["decision"] = {"id": action_id, "status": "error" if res["is_error"] else "done", "result": data}
    return JSONResponse(out)


async def _call_bifrost(token: str, tool: str, args: dict[str, Any]) -> dict[str, Any]:
    async with (
        httpx.AsyncClient(headers={"Authorization": f"Bearer {token}"}, timeout=httpx.Timeout(120)) as hc,
        streamable_http_client(BIFROST, http_client=hc) as st,
        ClientSession(st[0], st[1]) as cs,
    ):
        await cs.initialize()
        r = await cs.call_tool(tool, args)
    text = getattr(r.content[0], "text", "") if r.content else ""
    try:
        env = json.loads(text)
    except ValueError:
        env = {"error": text}
    is_err = bool(getattr(r, "is_error", None) or getattr(r, "isError", False))
    return {"is_error": is_err, "result": env}


async def upload(request: Request) -> Response:
    """Ask bifrost for a signed upload link; the browser then PUTs the file
    straight to Cloud Storage (the bytes never pass through the agent)."""
    w = _web(request)
    if not w:
        return JSONResponse({"error": "sign in first", "login": "/login"}, status_code=401)
    body = await request.json()
    name = str(body.get("filename", ""))[:200]
    try:
        size = int(body.get("bytes", 0))
    except (TypeError, ValueError):
        size = 0
    try:
        await _fresh_token(w)
        out = await _call_bifrost(w["token"], "upload_prepare", {"filename": name, "bytes": size})
    except PermissionError:
        WEB.pop(_sid(request) or "", None)
        return JSONResponse({"error": "sign-in expired", "login": "/login"}, status_code=401)
    if out["is_error"]:
        return JSONResponse({"error": str(out["result"].get("error", out["result"]))[:500]}, status_code=400)
    d = out["result"].get("data", {})
    return JSONResponse(
        {k: d.get(k) for k in ("upload_id", "filename", "upload_url", "headers", "method", "expires_at")}
    )


async def me(request: Request) -> Response:
    w = _web(request)
    if not w:
        return JSONResponse({"signed_in": False})
    s = await sessions.get_session(app_name=APP, user_id=w["email"], session_id=w["adk_session"])
    return JSONResponse(
        {
            "signed_in": True,
            "email": w["email"],
            "staff": "R2" in (w.get("tiers") or []),
            "version": __version__,
            "pending": approval.list_pending(s.state),
        }
    )


async def panel(request: Request) -> Response:
    """One dashboard panel: a read-only bifrost tool, cached per person (SPEC 20)."""
    w = _web(request)
    if not w:
        return JSONResponse({"error": "sign in first", "login": "/login"}, status_code=401)
    name = request.path_params["name"]
    q = {k: v for k, v in request.query_params.items() if k != "refresh"}
    try:
        p, args, key = panels.resolve(name, q)
    except KeyError:
        return JSONResponse({"error": f"no panel {name!r}"}, status_code=404)
    except panels.BadArgs as e:
        return JSONResponse({"error": str(e)}, status_code=400)
    if p.staff and "R2" not in (w.get("tiers") or []):
        return JSONResponse({"error": "staff panel (needs bifrost tier R2)"}, status_code=403)
    cache: panels.PanelCache = w.setdefault("cache", panels.PanelCache())

    async def call(tool: str, a: dict[str, Any]) -> dict[str, Any]:
        token = await _fresh_token(w)
        return await _call_bifrost(token, tool, a)

    try:
        out, meta = await cache.get(
            key, p.ttl, call, p.tool, args, force=request.query_params.get("refresh") == "1"
        )
    except PermissionError:
        WEB.pop(_sid(request) or "", None)
        return JSONResponse({"error": "sign-in expired", "login": "/login"}, status_code=401)
    except Exception as e:  # noqa: BLE001  bifrost unreachable or an MCP error: show it on this panel only
        root = e
        while isinstance(root, BaseExceptionGroup) and root.exceptions:  # anyio wraps the real error
            root = root.exceptions[0]
        log.warning("panel %s %s failed: %s: %s", w["email"], name, type(root).__name__, root)
        e = root
        return JSONResponse({"error": f"bifrost call failed: {type(e).__name__}"}, status_code=502)
    res = out.get("result")
    if out.get("is_error"):
        msg = str(res if isinstance(res, str) else (res or {}).get("error", res))
        if msg.startswith(UNREACHABLE):
            # bifrost could not log in to the cluster (connection being set up,
            # new login key propagating, cluster down): a passing state, so
            # say so plainly and let the page retry, instead of a raw SSH error
            log.info("panel %s %s: cluster unreachable: %s", w["email"], name, msg[:300])
            return JSONResponse(
                {"error": CONNECTING, "connecting": True, "detail": msg[:600], "tool": p.tool, "retry_s": 15},
                status_code=503,
            )
        return JSONResponse({"error": msg[:600], "tool": p.tool}, status_code=422)
    env = res if isinstance(res, dict) else {"data": res}
    return JSONResponse(
        {
            "panel": name,
            "tool": p.tool,
            "data": env.get("data"),
            "as_of": env.get("as_of"),
            "truncated": bool(env.get("truncated")),
            **meta,
        }
    )


async def new_chat(request: Request) -> Response:
    w = _web(request)
    if not w:
        return JSONResponse({"error": "sign in first"}, status_code=401)
    s = await sessions.create_session(app_name=APP, user_id=w["email"], state={TOKEN_KEY: w["token"]})
    w["adk_session"] = s.id
    return JSONResponse({"ok": True})


def _esc(s: str) -> str:
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;").replace('"', "&quot;")


STATIC = Path(__file__).parent / "static"
PAGE = (STATIC / "index.html").read_text()
# no inline script or style anywhere (SPEC 20.4): JS and CSS are files under /static
CSP = (
    "default-src 'self'; style-src 'self'; script-src 'self'; img-src 'self' data:; "
    "connect-src 'self' https://storage.googleapis.com; frame-ancestors 'none'; base-uri 'none'; "
    "form-action 'self'"
)
SEC_HEADERS = {
    "Content-Security-Policy": CSP,
    "X-Content-Type-Options": "nosniff",
    "Referrer-Policy": "no-referrer",
}


async def index(request: Request) -> Response:
    return HTMLResponse(PAGE, headers={**SEC_HEADERS, "Cache-Control": "no-store"})


class _Static(StaticFiles):
    """Static files with the same security headers as the page."""

    async def get_response(self, path: str, scope) -> Response:
        r = await super().get_response(path, scope)
        r.headers.update(SEC_HEADERS)
        r.headers["Cache-Control"] = "no-cache"
        return r


async def health(request: Request) -> Response:
    return JSONResponse({"status": "ok", "version": __version__})


# ---------------------------------------------------------------- A2A


class _A2AUser:
    """Starlette user for an A2A caller identified by a bifrost token."""

    is_authenticated = True

    def __init__(self, email: str):
        self.email = email

    @property
    def display_name(self) -> str:
        return self.email

    @property
    def identity(self) -> str:
        return self.email


async def _a2a_auth(scope, receive, send, inner):
    """ASGI guard for /a2a: the agent card is public; everything else needs a
    bifrost access token. The token is checked with bifrost (/whoami) and
    written into the caller's ADK session state before the agent runs, so tool
    calls carry it exactly like the chat page's."""
    from starlette.requests import Request as SRequest

    path = scope.get("path", "")
    if scope["type"] != "http" or path.endswith("/.well-known/agent-card.json"):
        return await inner(scope, receive, send)
    req = SRequest(scope, receive)
    tok = req.headers.get("authorization", "").removeprefix("Bearer ").strip()
    if not tok:
        res = JSONResponse(
            {"error": "bifrost access token required (Authorization: Bearer)"},
            status_code=401,
            headers={
                "WWW-Authenticate": f'Bearer resource_metadata="{BIFROST_BASE}/.well-known/oauth-protected-resource"'
            },
        )
        return await res(scope, receive, send)
    try:
        email = await _whoami(tok)
    except PermissionError:
        return await JSONResponse({"error": "invalid bifrost token"}, status_code=401)(scope, receive, send)
    scope["user"] = _A2AUser(email)
    scope.setdefault("state", {})["bifrost_token"] = tok
    A2A_TOKENS[email] = tok
    return await inner(scope, receive, send)


A2A_TOKENS: dict[str, str] = {}  # email -> latest bifrost token seen on /a2a


async def _a2a_before_agent(context):
    """A2A interceptor: put the caller's bifrost token into their session state."""
    user = getattr(getattr(context, "call_context", None), "user", None)
    email = getattr(user, "user_name", "") if user else ""
    tok = A2A_TOKENS.get(email)
    if tok and context.context_id:
        s = await sessions.get_session(app_name=APP, user_id=email, session_id=context.context_id)
        if s is None:
            await sessions.create_session(
                app_name=APP, user_id=email, session_id=context.context_id, state={TOKEN_KEY: tok}
            )
        else:
            from google.adk.events import Event, EventActions

            await sessions.append_event(
                s, Event(author="system", actions=EventActions(state_delta={TOKEN_KEY: tok}))
            )
    return context


def build_a2a_app():
    """A2A JSON-RPC endpoint + agent card under /a2a (Gemini Enterprise later).

    Callers send a bifrost access token (Authorization: Bearer ...), the same
    token an MCP client gets from bifrost's OAuth; it decides who the agent acts
    as. The approval gate holds over A2A too: plans come back with an approval
    id and only POST /a2a/decide (same bearer token) confirms. C5 will map
    Gemini Enterprise's user OAuth token to a bifrost session."""
    from a2a.types import AgentCapabilities, AgentSkill, HTTPAuthSecurityScheme, SecurityScheme
    from google.adk.a2a import _compat
    from google.adk.a2a.executor.a2a_agent_executor import A2aAgentExecutor
    from google.adk.a2a.executor.config import A2aAgentExecutorConfig, ExecuteInterceptor
    from google.adk.a2a.utils.agent_to_a2a import to_a2a

    skills = [
        AgentSkill(
            id="status",
            name="Cluster status and costs",
            description="What is running, the queue, partitions and prices.",
            tags=["hpc", "slurm"],
        ),
        AgentSkill(
            id="diagnose",
            name="Diagnose a failed job",
            description="Explains why a Slurm job failed and how to fix it.",
            tags=["slurm", "debugging"],
        ),
        AgentSkill(
            id="scripts",
            name="Write and check batch scripts",
            description="Drafts Slurm scripts and checks them against Ursa Major.",
            tags=["slurm"],
        ),
        AgentSkill(
            id="submit",
            name="Plan job submissions",
            description="Plans submit/cancel/hold/release; the user approves each one.",
            tags=["slurm"],
        ),
    ]
    bearer = SecurityScheme(
        http_auth_security_scheme=HTTPAuthSecurityScheme(
            scheme="bearer",
            description=f"bifrost access token (OAuth 2.1 via {BIFROST_BASE}; Google sign-in, ucr.edu)",
        )
    )
    card = _compat.build_agent_card(
        name="Ursa Major assistant",
        description=agent.description,
        version=__version__,
        url=f"{BASE_URL}/a2a/",
        protocol_binding=getattr(_compat.TP_JSONRPC, "value", _compat.TP_JSONRPC),
        skills=skills,
        capabilities=AgentCapabilities(streaming=True),
        provider=None,
        security_schemes={"bifrost": bearer},
        doc_url="https://github.com/UCR-Research-Computing/ursa-bifrost",
        default_input_modes=["text/plain"],
        default_output_modes=["text/plain"],
        supports_authenticated_extended_card=False,
    )
    cfg = A2aAgentExecutorConfig(execute_interceptors=[ExecuteInterceptor(before_agent=_a2a_before_agent)])
    a2a_runner = Runner(app_name=APP, agent=agent, session_service=sessions)
    inner = to_a2a(
        agent,
        agent_card=card,
        runner=a2a_runner,
        agent_executor_factory=lambda r: A2aAgentExecutor(runner=r, config=cfg),
    )

    async def guarded(scope, receive, send):
        return await _a2a_auth(scope, receive, send, inner)

    return inner, guarded


async def a2a_decide(request: Request) -> Response:
    """Approve/reject over A2A (bearer token instead of the chat cookie)."""
    tok = request.headers.get("authorization", "").removeprefix("Bearer ").strip()
    try:
        email = await _whoami(tok)
    except PermissionError:
        return JSONResponse({"error": "invalid bifrost token"}, status_code=401)
    body = await request.json()
    session_id, action_id, verdict = (
        str(body.get("context_id", "")),
        str(body.get("id", "")),
        body.get("decision"),
    )
    if verdict not in ("approve", "reject"):
        return JSONResponse({"error": "decision must be approve or reject"}, status_code=400)
    s = await sessions.get_session(app_name=APP, user_id=email, session_id=session_id)
    if s is None:
        return JSONResponse({"error": "unknown context_id for this user"}, status_code=404)
    state = dict(s.state)
    pend = approval.take(state, action_id)
    if not pend:
        return JSONResponse({"error": "no such pending action"}, status_code=404)
    from google.adk.events import Event, EventActions

    await sessions.append_event(
        s,
        Event(
            author="system", actions=EventActions(state_delta={approval.STATE_KEY: state[approval.STATE_KEY]})
        ),
    )
    if verdict == "reject":
        return JSONResponse({"id": action_id, "status": "rejected"})
    res = await _confirm_with_bifrost(tok, pend.confirm_tool, pend.token)
    log.info("a2a approve %s %s -> error=%s", email, pend.confirm_tool, res["is_error"])
    data = res["result"].get("data", res["result"]) if isinstance(res["result"], dict) else res["result"]
    return JSONResponse({"id": action_id, "status": "error" if res["is_error"] else "done", "result": data})


A2A_INNER, A2A_APP = build_a2a_app()


@asynccontextmanager
async def lifespan(app: Starlette):
    log.info("ursa-agent %s at %s, bifrost %s", __version__, BASE_URL, BIFROST)
    # the A2A sub-app attaches its routes in its own lifespan
    async with A2A_INNER.router.lifespan_context(A2A_INNER):
        yield


routes = [
    Route("/", index),
    Route("/health", health),
    Route("/login", login),
    Route("/auth/callback", callback),
    Route("/logout", logout),
    Route("/api/me", me),
    Route("/api/chat", chat, methods=["POST"]),
    Route("/api/decide", decide, methods=["POST"]),
    Route("/api/new", new_chat, methods=["POST"]),
    Route("/api/upload", upload, methods=["POST"]),
    Route("/api/panel/{name}", panel),
    Mount("/static", app=_Static(directory=STATIC), name="static"),
    Route("/a2a/decide", a2a_decide, methods=["POST"]),
    Mount("/a2a", app=A2A_APP),
]

app = Starlette(routes=routes, lifespan=lifespan)
