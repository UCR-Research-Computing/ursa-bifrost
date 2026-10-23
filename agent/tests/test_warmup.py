"""The LiteLLM warm-up runs offline, never raises, and can be switched off."""

import socket

from ursa_agent import web


def test_warm_up_is_offline(monkeypatch):
    def no_network(*a, **k):
        raise AssertionError("warm-up must not open a network connection")

    monkeypatch.setattr(socket.socket, "connect", no_network)
    assert web.warm_up() >= 0


def test_warm_up_never_raises(monkeypatch):
    import litellm

    async def boom(*a, **k):
        raise RuntimeError("broken model library")

    monkeypatch.setattr(litellm, "acompletion", boom)
    assert web.warm_up() >= 0


def test_lifespan_skips_when_off(monkeypatch):
    from starlette.testclient import TestClient

    calls = []
    monkeypatch.setattr(web, "warm_up", lambda: calls.append(1) or 0.1)
    monkeypatch.setenv("URSA_AGENT_WARMUP", "0")
    with TestClient(web.app):
        pass
    assert calls == []
    monkeypatch.delenv("URSA_AGENT_WARMUP")
    with TestClient(web.app):
        pass
    assert calls == [1]
