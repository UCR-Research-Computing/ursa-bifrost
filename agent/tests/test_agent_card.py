"""The public A2A agent card: fields both A2A 1.0 and 0.3 clients (Gemini Enterprise) read."""

import os

os.environ.setdefault("AGENT_SESSION_SECRET", "test-secret")
os.environ.setdefault("GATEWAY_API_KEY", "test-key")

import pytest
from starlette.testclient import TestClient

from ursa_agent import __version__, web

CARD = "/a2a/.well-known/agent-card.json"


@pytest.fixture(scope="module")
def client():
    with TestClient(web.app) as c:  # lifespan attaches the A2A routes
        yield c


@pytest.fixture(scope="module")
def card(client):
    r = client.get(CARD)
    assert r.status_code == 200
    return r.json()


def test_card_is_public_and_versioned(card):
    assert card["name"] == "Ursa Major assistant"
    assert card["version"] == __version__


def test_card_has_top_level_url_and_protocol_version(card):
    # A2A 0.3 clients read these top-level fields; 1.0 keeps them in supportedInterfaces.
    assert card["url"] == f"{web.BASE_URL}/a2a/"
    assert card["protocolVersion"].startswith("0.3")
    assert card["preferredTransport"] == "JSONRPC"


def test_card_supported_interfaces_cover_1_0_and_0_3(card):
    ifaces = card["supportedInterfaces"]
    assert [i["protocolVersion"] for i in ifaces] == ["1.0", "0.3.0"]
    for i in ifaces:
        assert i["url"] == f"{web.BASE_URL}/a2a/"
        assert i["protocolBinding"] == "JSONRPC"


def test_card_skills(card):
    assert [s["id"] for s in card["skills"]] == ["status", "diagnose", "scripts", "submit"]
    assert all(s["name"] and s["description"] and s["tags"] for s in card["skills"])


def test_card_security_scheme_is_bifrost_bearer(card):
    scheme = card["securitySchemes"]["bifrost"]
    # 1.0 shape and 0.3 shape (merged into the same object by the SDK)
    assert scheme["httpAuthSecurityScheme"]["scheme"] == "bearer"
    assert scheme["type"] == "http" and scheme["scheme"] == "bearer"
    assert web.BIFROST_BASE in scheme["httpAuthSecurityScheme"]["description"]


def test_card_url_is_the_rpc_route_without_a_redirect(client, card):
    path = card["url"].removeprefix(web.BASE_URL)
    body = {"jsonrpc": "2.0", "id": 1, "method": "message/send", "params": {}}
    r = client.post(path, json=body, follow_redirects=False)
    assert r.status_code == 401  # reached the A2A guard: needs a bifrost token, not a 307
