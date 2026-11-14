"""Security and UI integration tests for ursa-agent web & A2A routes (P2-05, P2-16, P3-11)."""

from __future__ import annotations

import asyncio
import os
import time
from types import SimpleNamespace
from urllib.parse import parse_qs, urlparse

os.environ.setdefault("AGENT_SESSION_SECRET", "test-secret")
os.environ.setdefault("GATEWAY_API_KEY", "test-key")

import pytest
from google.adk.sessions import InMemorySessionService
from starlette.exceptions import HTTPException
from starlette.routing import Route
from starlette.testclient import TestClient

from ursa_agent import web
from ursa_agent.agent import TOKEN_KEY


# ---------------------------------------------------------------- P2-16: ContextVar A2A tokens
async def test_concurrent_a2a_requests_same_email_keep_isolated_tokens(monkeypatch):
    """Two concurrent A2A calls for the same email must never overwrite each other's token."""
    assert not hasattr(web, "A2A_TOKENS")
    fresh_sessions = InMemorySessionService()
    monkeypatch.setattr(web, "sessions", fresh_sessions)

    async def fake_whoami(tok: str) -> str:
        assert tok in ("tok-alpha", "tok-beta")
        return "alice@ucr.edu"

    monkeypatch.setattr(web, "_whoami", fake_whoami)

    both_inside = asyncio.Event()
    entered = 0

    async def fake_inner(scope, receive, send):
        nonlocal entered
        tok = scope["state"]["bifrost_token"]
        ctx_id = f"ctx-{tok}"
        entered += 1
        if entered == 2:
            both_inside.set()
        await both_inside.wait()
        # Both requests have now run _a2a_auth for alice@ucr.edu before either runs _a2a_before_agent.
        ctx = SimpleNamespace(
            context_id=ctx_id,
            call_context=SimpleNamespace(user=SimpleNamespace(user_name="alice@ucr.edu")),
        )
        await web._a2a_before_agent(ctx)

    def make_scope(tok: str) -> dict:
        return {
            "type": "http",
            "method": "POST",
            "path": "/a2a/",
            "headers": [(b"authorization", f"Bearer {tok}".encode())],
        }

    async def noop_receive():
        return {"type": "http.request", "body": b""}

    async def noop_send(msg):
        return None

    await asyncio.gather(
        web._a2a_auth(make_scope("tok-alpha"), noop_receive, noop_send, fake_inner),
        web._a2a_auth(make_scope("tok-beta"), noop_receive, noop_send, fake_inner),
    )

    assert web._a2a_tok.get() == ""
    s_alpha = await web.sessions.get_session(
        app_name=web.APP, user_id="alice@ucr.edu", session_id="ctx-tok-alpha"
    )
    s_beta = await web.sessions.get_session(
        app_name=web.APP, user_id="alice@ucr.edu", session_id="ctx-tok-beta"
    )
    assert s_alpha is not None and s_alpha.state[TOKEN_KEY] == "tok-alpha"
    assert s_beta is not None and s_beta.state[TOKEN_KEY] == "tok-beta"


# ---------------------------------------------------------------- P2-05: Login CSRF + POST Origin + POST /logout
def test_login_sets_state_cookie_and_callback_verifies_it(monkeypatch):
    web.CLIENT["id"] = "bfc_test"
    web.PENDING_LOGIN.clear()

    # HTTPS BASE_URL marks cookie Secure; HTTP BASE_URL omits Secure.
    monkeypatch.setattr(web, "BASE_URL", "https://agent.example.edu")
    c_https = TestClient(web.app, base_url="https://agent.example.edu")
    r_login = c_https.get("/auth/login", follow_redirects=False)
    assert r_login.status_code == 302
    set_ck = r_login.headers["set-cookie"]
    assert f"{web.STATE_COOKIE}=" in set_ck
    assert "HttpOnly" in set_ck and "SameSite=lax" in set_ck and "Secure" in set_ck
    assert "Path=/auth/callback" in set_ck

    monkeypatch.setattr(web, "BASE_URL", "http://localhost:8080")
    c = TestClient(web.app)
    r = c.get("/login", follow_redirects=False)
    assert r.status_code == 302
    assert "Secure" not in r.headers["set-cookie"]
    state = parse_qs(urlparse(r.headers["location"]).query)["state"][0]

    # Login CSRF attempt: another browser (no matching state cookie) hits /auth/callback with attacker's state.
    victim = TestClient(web.app)
    r_csrf = victim.get(f"/auth/callback?code=att-code&state={state}", follow_redirects=False)
    assert r_csrf.status_code == 400
    assert web.COOKIE not in r_csrf.cookies
    # Legitimate pending login was not consumed by the unauthenticated browser.
    assert state in web.PENDING_LOGIN

    # Matching browser cookie completes sign-in cleanly.
    class TokenResp:
        status_code = 200

        @staticmethod
        def json():
            return {"access_token": "bfx_ok", "refresh_token": "bfr_ok", "expires_in": 3600}

    class FakeHTTP:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a):
            return False

        async def post(self, url, data=None, **k):
            assert data is not None and data["grant_type"] == "authorization_code"
            return TokenResp()

    async def fake_whoami_full(tok: str):
        assert tok == "bfx_ok"
        return "alice@ucr.edu", ["R1"]

    monkeypatch.setattr(web.httpx, "AsyncClient", FakeHTTP)
    monkeypatch.setattr(web, "_whoami_full", fake_whoami_full)

    r_ok = c.get(f"/auth/callback?code=good-code&state={state}", follow_redirects=False)
    assert r_ok.status_code == 302 and r_ok.headers["location"] == "/"
    assert f"{web.COOKIE}=" in r_ok.headers.get("set-cookie", "")


@pytest.mark.parametrize(
    "path,payload",
    [
        ("/api/chat", {"message": "hello"}),
        ("/api/decide", {"id": "a1", "decision": "reject"}),
        ("/api/new", {}),
        ("/api/upload", {"filename": "in.txt", "bytes": 10}),
    ],
)
def test_post_api_routes_enforce_same_origin(monkeypatch, path, payload):
    sid = "sid-origin"
    web.WEB[sid] = {
        "email": "alice@ucr.edu",
        "tiers": ["R1"],
        "token": "tok-1",
        "refresh": "r-1",
        "exp": time.time() + 3600,
        "adk_session": None,
        "client_id": "c",
    }
    c = TestClient(web.app)
    c.cookies.set(web.COOKIE, web.signer.dumps(sid))

    for bad_headers in ({}, {"Origin": "https://evil.example"}, {"Origin": "null"}):
        r = c.post(path, json=payload, headers=bad_headers)
        assert r.status_code == 403, (path, bad_headers, r.status_code)

    if path == "/api/new":
        r_good = c.post(path, json=payload, headers={"Origin": web.BASE_URL})
        assert r_good.status_code == 200 and r_good.json() == {"ok": True}
    web.WEB.clear()


def test_index_uses_post_logout_form_and_referrer_same_origin():
    html = (web.STATIC / "index.html").read_text()
    assert 'href="/logout"' not in html
    assert '<form method="post" action="/logout"' in html
    c = TestClient(web.app)
    r = c.get("/")
    assert r.headers["referrer-policy"] == "same-origin"


# ---------------------------------------------------------------- P3-11: Styled 403 & optional ATRIUM_URL
def test_atrium_url_optional_and_omitted_when_unset(monkeypatch):
    c = TestClient(web.app)

    monkeypatch.delenv("ATRIUM_URL", raising=False)
    monkeypatch.setattr(web, "ATRIUM_URL", "")
    r_empty = c.get("/")
    assert r_empty.status_code == 200
    assert "util-atrium" not in r_empty.text
    assert "Atrium" not in r_empty.text
    assert "run.app" not in r_empty.text

    monkeypatch.setenv("ATRIUM_URL", "https://atrium.example.edu/portal")
    r_set = c.get("/")
    assert r_set.status_code == 200
    assert '<a href="https://atrium.example.edu/portal" class="util-atrium">Atrium</a>' in r_set.text
    assert 'href="https://atrium.example.edu/portal">Atrium</a>' in r_set.text


def test_403_not_allowed_page_is_styled_and_escapes_html(monkeypatch):
    monkeypatch.setenv("ATRIUM_URL", "https://atrium.example.edu/")
    web.CLIENT["id"] = "bfc_test"
    c = TestClient(web.app)
    r_login = c.get("/login", follow_redirects=False)
    state = parse_qs(urlparse(r_login.headers["location"]).query)["state"][0]

    r = c.get(
        f"/auth/callback?state={state}&error=access_denied&error_description=<script>alert(1)</script>+unlisted",
        follow_redirects=False,
    )
    assert r.status_code == 403
    assert "/static/app.css" in r.text
    assert "<h1>Not allowed</h1>" in r.text
    assert "<script>alert(1)</script>" not in r.text
    assert "&lt;script&gt;alert(1)&lt;/script&gt;" in r.text
    assert "https://atrium.example.edu/" in r.text
    assert "style-src 'self'" in r.headers["content-security-policy"]

    # Exception handler for generic 403 HTML routes also renders styled Not allowed page.
    def deny(_request):
        raise HTTPException(status_code=403, detail="Not allowed")

    web.app.router.routes.append(Route("/_test_403", deny))
    try:
        r_exc = c.get("/_test_403")
        assert r_exc.status_code == 403
        assert "/static/app.css" in r_exc.text and "<h1>Not allowed</h1>" in r_exc.text
    finally:
        web.app.router.routes.pop()
