"""Sign-ins survive a restart (ursa-agent 0.4.0): store.py and its use in web.py."""

import json
import os
import time

os.environ.setdefault("AGENT_SESSION_SECRET", "test-secret")
os.environ.setdefault("GATEWAY_API_KEY", "test-key")

import pytest
from starlette.testclient import TestClient

from ursa_agent import web
from ursa_agent.store import SessionStore

REC = {"email": "a@ucr.edu", "tiers": ["R1"], "token": "at-1", "refresh": "rt-1", "exp": time.time() + 3600,
       "client_id": "bfc_x", "created": time.time()}  # fmt: skip


def test_round_trip_and_only_known_fields(tmp_path):
    s = SessionStore(tmp_path, "k")
    s.save("sid-1", {**REC, "cache": object(), "lock": object(), "adk_session": "x", "note": "chat text"})
    got = s.load("sid-1")
    assert got == {k: REC[k] for k in REC}
    written = json.loads(s.f.decrypt(s._path("sid-1").read_bytes()))
    assert set(written) == set(REC)  # nothing but the allow-listed fields reaches the bucket


def test_file_is_sealed_and_named_by_hash(tmp_path):
    s = SessionStore(tmp_path, "k")
    s.save("sid-secret", REC)
    files = list((tmp_path / "sessions").iterdir())
    assert len(files) == 1 and "sid-secret" not in files[0].name
    raw = files[0].read_bytes()
    assert b"rt-1" not in raw and b"a@ucr.edu" not in raw


def test_other_secret_cannot_read_and_bad_record_is_removed(tmp_path):
    SessionStore(tmp_path, "k").save("sid-1", REC)
    other = SessionStore(tmp_path, "different")
    assert other.load("sid-1") is None
    assert not list((tmp_path / "sessions").glob("*.bin"))


def test_expired_record_is_refused(tmp_path):
    s = SessionStore(tmp_path, "k", max_age_s=60)
    s.save("sid-1", {**REC, "created": time.time() - 120})
    assert s.load("sid-1") is None


def test_sweep_removes_old_files(tmp_path):
    s = SessionStore(tmp_path, "k", max_age_s=60)
    s.save("old", REC)
    s.save("new", REC)
    old = s._path("old")
    os.utime(old, (time.time() - 120, time.time() - 120))
    assert s.sweep() == 1 and not old.exists() and s._path("new").exists()


def test_disabled_without_a_directory():
    s = SessionStore(None, "k")
    s.save("sid", REC)
    assert not s.enabled and s.load("sid") is None


def test_client_registration_kept_per_base_url(tmp_path):
    s = SessionStore(tmp_path, "k")
    s.save_client("https://a|https://b", "bfc_1")
    assert s.load_client("https://a|https://b") == "bfc_1"
    assert s.load_client("https://other|https://b") is None


# ---- web.py: a restart (memory wiped) keeps the person signed in ------------------


@pytest.fixture
def app(tmp_path, monkeypatch):
    store = SessionStore(tmp_path, "test-secret")
    monkeypatch.setattr(web, "STORE", store)
    web.WEB.clear()
    calls = []

    async def fake_call(token, tool, args):
        calls.append((token, tool))
        return {"is_error": False, "result": {"as_of": "t", "data": {"ok": True}}}

    monkeypatch.setattr(web, "_call_bifrost", fake_call)
    yield store, calls
    web.WEB.clear()


def _client(sid):
    c = TestClient(web.app)
    c.cookies.set(web.COOKIE, web.signer.dumps(sid))
    return c


def test_restart_keeps_sign_in(app):
    store, calls = app
    store.save("sid-a", REC)
    web.WEB.clear()  # the restart
    c = _client("sid-a")
    me = c.get("/api/me").json()
    assert me["signed_in"] and me["email"] == "a@ucr.edu"
    r = c.get("/api/panel/pulse")
    assert r.status_code == 200 and calls[-1][0] == "at-1"


def test_rotated_refresh_token_is_stored(app, monkeypatch):
    store, _ = app
    store.save("sid-a", {**REC, "exp": time.time() - 1})
    web.WEB.clear()

    class R:
        status_code = 200

        @staticmethod
        def json():
            return {"access_token": "at-2", "refresh_token": "rt-2", "expires_in": 3600}

    class H:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a):
            return False

        async def post(self, url, data=None, **k):
            assert data["refresh_token"] == "rt-1"
            return R()

    monkeypatch.setattr(web.httpx, "AsyncClient", H)
    c = _client("sid-a")
    assert c.get("/api/panel/pulse").status_code == 200
    web.WEB.clear()  # another restart: the NEW refresh token must be the stored one
    assert store.load("sid-a")["refresh"] == "rt-2"


def test_logout_deletes_the_stored_sign_in(app, monkeypatch):
    store, _ = app
    store.save("sid-a", REC)
    web.WEB.clear()

    class H:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a):
            return False

        async def post(self, *a, **k):
            return None

    monkeypatch.setattr(web.httpx, "AsyncClient", H)
    c = _client("sid-a")
    c.get("/logout", follow_redirects=False)
    assert store.load("sid-a") is None
    web.WEB.clear()
    assert _client("sid-a").get("/api/me").json() == {"signed_in": False}


def test_refused_refresh_forgets_the_stored_sign_in(app, monkeypatch):
    store, _ = app
    store.save("sid-a", {**REC, "exp": time.time() - 1})
    web.WEB.clear()

    class R:
        status_code = 400

    class H:
        def __init__(self, *a, **k):
            pass

        async def __aenter__(self):
            return self

        async def __aexit__(self, *a):
            return False

        async def post(self, *a, **k):
            return R()

    monkeypatch.setattr(web.httpx, "AsyncClient", H)
    r = _client("sid-a").get("/api/panel/pulse")
    assert r.status_code == 401 and store.load("sid-a") is None


def test_forged_cookie_finds_nothing(app):
    store, _ = app
    store.save("sid-a", REC)
    web.WEB.clear()
    c = TestClient(web.app)
    c.cookies.set(web.COOKIE, "sid-a")  # unsigned
    assert c.get("/api/me").json() == {"signed_in": False}
