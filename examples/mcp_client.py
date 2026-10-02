#!/usr/bin/env python3
"""Minimal script client for a hosted bifrost (or any MCP server with OAuth 2.1 +
dynamic client registration, like the Nexus MCP server).

  python3 mcp_client.py login                 # once: browser sign-in, tokens saved (0600)
  python3 mcp_client.py tools                 # list tools
  python3 mcp_client.py call cluster_status   # call a tool
  python3 mcp_client.py call job_explain '{"job_id": "236"}'

Environment:
  MCP_URL     server base URL (default: the Ursa Major bifrost)
  MCP_TOKENS  token file (default: ~/.config/mcp-client/<host>.json)

Needs only the Python standard library. It speaks JSON-RPC over Streamable HTTP
directly, so it is also a readable reference for what an MCP client does.
"""

from __future__ import annotations

import base64
import hashlib
import http.server
import json
import os
import secrets
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import webbrowser

BASE = os.environ.get(
    "MCP_URL", "https://bifrost-mcp-125853442225.us-central1.run.app"
).rstrip("/")
HOST = urllib.parse.urlparse(BASE).hostname or "server"
TOKENS = os.path.expanduser(
    os.environ.get("MCP_TOKENS", f"~/.config/mcp-client/{HOST}.json")
)
PORT = 47614
REDIRECT = f"http://127.0.0.1:{PORT}/callback"


def _http(method: str, url: str, data: bytes | None = None, headers: dict | None = None):
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            return r.status, dict(r.headers), r.read()
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read()


def _save(tokens: dict) -> None:
    os.makedirs(os.path.dirname(TOKENS), exist_ok=True)
    fd = os.open(TOKENS, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as f:
        json.dump(tokens, f)


def login() -> None:
    meta = json.loads(_http("GET", BASE + "/.well-known/oauth-authorization-server")[2])
    status, _, body = _http(
        "POST",
        meta["registration_endpoint"],
        json.dumps(
            {
                "client_name": "mcp-client.py",
                "redirect_uris": [REDIRECT],
                "token_endpoint_auth_method": "none",
            }
        ).encode(),
        {"Content-Type": "application/json"},
    )
    if status not in (200, 201):
        sys.exit(f"registration failed: {status} {body[:200]!r}")
    client_id = json.loads(body)["client_id"]
    verifier = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
    challenge = (
        base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest())
        .rstrip(b"=")
        .decode()
    )
    state = secrets.token_urlsafe(16)
    url = meta["authorization_endpoint"] + "?" + urllib.parse.urlencode(
        {
            "response_type": "code",
            "client_id": client_id,
            "redirect_uri": REDIRECT,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
            "state": state,
        }
    )
    got: dict = {}

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            q = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
            got.update({k: v[0] for k, v in q.items()})
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"Signed in. You can close this tab.")

        def log_message(self, format, *args):  # noqa: A002
            pass

    srv = http.server.HTTPServer(("127.0.0.1", PORT), Handler)
    threading.Thread(target=srv.handle_request, daemon=True).start()
    print("Opening your browser to sign in. If it doesn't open, visit:\n" + url)
    webbrowser.open(url)
    for _ in range(600):
        if got:
            break
        time.sleep(0.5)
    if got.get("state") != state or "code" not in got:
        sys.exit(f"sign-in failed: {got.get('error') or 'no code returned'}")
    status, _, body = _http(
        "POST",
        meta["token_endpoint"],
        urllib.parse.urlencode(
            {
                "grant_type": "authorization_code",
                "code": got["code"],
                "redirect_uri": REDIRECT,
                "client_id": client_id,
                "code_verifier": verifier,
            }
        ).encode(),
        {"Content-Type": "application/x-www-form-urlencoded"},
    )
    if status != 200:
        sys.exit(f"token exchange failed: {status} {body[:200]!r}")
    tok = json.loads(body)
    tok.update(client_id=client_id, token_endpoint=meta["token_endpoint"])
    tok["_exp"] = time.time() + tok.get("expires_in", 3600)
    _save(tok)
    print(f"Signed in. Tokens saved to {TOKENS}")


def access_token() -> str:
    try:
        tok = json.load(open(TOKENS))
    except FileNotFoundError:
        sys.exit("not signed in: run `mcp_client.py login` first")
    if tok.get("_exp", 0) - time.time() > 60:
        return tok["access_token"]
    status, _, body = _http(
        "POST",
        tok["token_endpoint"],
        urllib.parse.urlencode(
            {
                "grant_type": "refresh_token",
                "refresh_token": tok["refresh_token"],
                "client_id": tok["client_id"],
            }
        ).encode(),
        {"Content-Type": "application/x-www-form-urlencoded"},
    )
    if status != 200:
        sys.exit("session expired: run `mcp_client.py login` again")
    new = json.loads(body)
    tok.update(new)  # refresh tokens rotate: always keep the newest
    tok["_exp"] = time.time() + new.get("expires_in", 3600)
    _save(tok)
    return tok["access_token"]


def rpc(method: str, params: dict | None = None) -> dict:
    """One JSON-RPC call over Streamable HTTP (stateless servers need no session)."""
    status, headers, body = _http(
        "POST",
        BASE + "/mcp",
        json.dumps(
            {"jsonrpc": "2.0", "id": 1, "method": method, "params": params or {}}
        ).encode(),
        {
            "Authorization": "Bearer " + access_token(),
            "Content-Type": "application/json",
            "Accept": "application/json, text/event-stream",
        },
    )
    if status != 200:
        sys.exit(f"{method}: HTTP {status} {body[:300]!r}")
    text = body.decode()
    if "text/event-stream" in headers.get("Content-Type", headers.get("content-type", "")):
        text = "\n".join(
            line[5:].strip() for line in text.splitlines() if line.startswith("data:")
        )
    msg = json.loads(text.splitlines()[-1] if "\n" in text else text)
    if "error" in msg:
        sys.exit(f"{method}: {msg['error']}")
    return msg["result"]


def main() -> None:
    cmd = sys.argv[1] if len(sys.argv) > 1 else "help"
    if cmd == "login":
        login()
    elif cmd == "tools":
        for t in rpc("tools/list")["tools"]:
            ro = (t.get("annotations") or {}).get("readOnlyHint")
            desc = (t.get("description") or "").strip().splitlines()[0][:70]
            print(f"{t['name']:26} {'read ' if ro else 'write'}  {desc}")
    elif cmd == "call" and len(sys.argv) >= 3:
        args = json.loads(sys.argv[3]) if len(sys.argv) > 3 else {}
        res = rpc("tools/call", {"name": sys.argv[2], "arguments": args})
        for part in res.get("content", []):
            if part.get("type") == "text":
                print(part["text"])
        if res.get("isError"):
            sys.exit(1)
    else:
        print(__doc__)


if __name__ == "__main__":
    main()
