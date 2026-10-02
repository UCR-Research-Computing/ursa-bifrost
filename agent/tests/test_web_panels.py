"""The /api/panel endpoint and the page headers, with a fake bifrost (no network)."""

import asyncio
import os
import time

os.environ.setdefault("AGENT_SESSION_SECRET", "test-secret")
os.environ.setdefault("GATEWAY_API_KEY", "test-key")

import pytest
from starlette.testclient import TestClient

from ursa_agent import web

SID = "sid-test"


@pytest.fixture
def client(monkeypatch):
    calls = []

    async def fake_call(token, tool, args):
        calls.append((token, tool, dict(args)))
        if tool == "job_show" and args.get("job_id") == "404":
            return {"is_error": True, "result": 'job id "404": not found'}
        return {
            "is_error": False,
            "result": {"as_of": "2026-10-02T16:00:00Z", "data": {"tool": tool, "args": args}},
        }

    monkeypatch.setattr(web, "_call_bifrost", fake_call)
    web.WEB.clear()
    web.WEB[SID] = {
        "email": "someone@ucr.edu",
        "tiers": ["R1"],
        "token": "tok-1",
        "refresh": "r-1",
        "exp": time.time() + 3600,
        "adk_session": "s",
        "client_id": "c",
    }
    c = TestClient(web.app)
    c.cookies.set(web.COOKIE, web.signer.dumps(SID))
    c.calls = calls
    yield c
    web.WEB.clear()


def test_panel_needs_sign_in():
    c = TestClient(web.app)
    r = c.get("/api/panel/pulse")
    assert r.status_code == 401 and r.json()["login"] == "/login"


def test_panel_returns_data_with_the_persons_token(client):
    r = client.get("/api/panel/jobs?range=30d")
    assert r.status_code == 200
    j = r.json()
    assert j["tool"] == "jobs_list" and j["data"]["args"] == {"since": "now-30days", "limit": 50}
    assert j["as_of"] == "2026-10-02T16:00:00Z" and j["cached"] is False
    assert client.calls == [("tok-1", "jobs_list", {"since": "now-30days", "limit": 50})]
    # second request is served from the per-person cache
    assert client.get("/api/panel/jobs?range=30d").json()["cached"] is True and len(client.calls) == 1


def test_bad_args_are_400_and_never_reach_bifrost(client):
    r = client.get("/api/panel/job?job=1;ls")
    assert r.status_code == 400 and client.calls == []


def test_unknown_or_write_panel_is_404(client):
    for name in ("job_submit", "job_cancel_confirm", "files_read", "nope"):
        assert client.get(f"/api/panel/{name}").status_code == 404
    assert client.calls == []


def test_staff_panel_refused_without_r2(client):
    r = client.get("/api/panel/health")
    assert r.status_code == 403 and client.calls == []


def test_staff_panel_allowed_with_r2(client):
    web.WEB[SID]["tiers"] = ["R1", "R2"]
    r = client.get("/api/panel/usersusage?range=7d")
    assert r.status_code == 200 and client.calls[-1][1] == "usage_report"
    assert client.calls[-1][2] == {"since": "now-7days", "group_by": "user"}


def test_bifrost_error_is_422_with_its_message(client):
    r = client.get("/api/panel/job?job=404")
    assert r.status_code == 422 and "not found" in r.json()["error"]


def test_caches_are_per_person(client):
    client.get("/api/panel/pulse")
    other = "sid-other"
    web.WEB[other] = dict(web.WEB[SID], email="other@ucr.edu", token="tok-2")
    web.WEB[other].pop("cache", None)
    c2 = TestClient(web.app)
    c2.cookies.set(web.COOKIE, web.signer.dumps(other))
    c2.get("/api/panel/pulse")
    assert [t for t, *_ in client.calls] == ["tok-1", "tok-2"]


def test_me_reports_staff_from_tiers(client):
    async def fake_get_session(**kw):
        return type("S", (), {"state": {}})()

    web.sessions.get_session = fake_get_session  # type: ignore[method-assign]
    assert client.get("/api/me").json()["staff"] is False
    web.WEB[SID]["tiers"] = ["R2"]
    assert client.get("/api/me").json()["staff"] is True


def test_page_and_static_send_strict_csp(client):
    for path in ("/", "/static/app.js", "/static/app.css"):
        r = client.get(path)
        assert r.status_code == 200, path
        csp = r.headers["content-security-policy"]
        assert "'unsafe-inline'" not in csp and "script-src 'self'" in csp and "style-src 'self'" in csp
        assert r.headers["x-content-type-options"] == "nosniff"


def test_static_does_not_serve_outside_its_folder(client):
    for path in ("/static/../web.py", "/static/%2e%2e/web.py", "/static/../../pyproject.toml"):
        r = client.get(path)
        assert r.status_code == 404, path


# ---------------------------------------------------------------- token refresh under load
async def test_parallel_refresh_uses_the_refresh_token_once(monkeypatch):
    """bifrost rotates refresh tokens: two refreshes with the same one would sign
    the person out. Parallel panel loads must refresh once and share the result."""
    posts = []

    class Resp:
        status_code = 200

        def json(self):
            return {
                "access_token": f"new-{len(posts)}",
                "refresh_token": f"r-{len(posts) + 1}",
                "expires_in": 3600,
            }

    class FakeHTTP:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a):
            return False

        async def post(self, url, data=None, **k):
            posts.append(data["refresh_token"])
            await asyncio.sleep(0.01)
            return Resp()

    async def no_state(w, delta):
        return None

    monkeypatch.setattr(web.httpx, "AsyncClient", FakeHTTP)
    monkeypatch.setattr(web, "_set_state", no_state)
    w = {"email": "x@ucr.edu", "token": "old", "refresh": "r-1", "exp": time.time() + 10, "client_id": "c"}
    toks = await asyncio.gather(*[web._fresh_token(w) for _ in range(6)])
    assert posts == ["r-1"], posts
    assert set(toks) == {"new-1"} and w["refresh"] == "r-2"
