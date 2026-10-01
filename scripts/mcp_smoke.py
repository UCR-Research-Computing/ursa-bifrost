#!/usr/bin/env python3
"""Drive `bifrost mcp` over stdio like a real MCP client (initialize, list, call)."""

import json
import subprocess
import sys

BIN = sys.argv[1] if len(sys.argv) > 1 else "bifrost"
p = subprocess.Popen([BIN, "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)


def send(msg):
    p.stdin.write(json.dumps(msg) + "\n")
    p.stdin.flush()


def recv(want_id):
    while True:
        line = p.stdout.readline()
        if not line:
            raise SystemExit("server closed")
        m = json.loads(line)
        if m.get("id") == want_id:
            return m


send({"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
    "protocolVersion": "2025-06-18", "capabilities": {},
    "clientInfo": {"name": "stdio-smoke", "version": "0"}}})
init = recv(1)["result"]
print("server:", init["serverInfo"]["name"], init["serverInfo"]["version"], "protocol", init["protocolVersion"])
send({"jsonrpc": "2.0", "method": "notifications/initialized"})
send({"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
tools = recv(2)["result"]["tools"]
print("tools:", ", ".join(t["name"] for t in tools))
send({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "job_explain", "arguments": {"job_id": "203"}}})
r = recv(3)["result"]
d = r["structuredContent"]["data"]
print("job_explain 203 ->", [f["rule"] for f in d["findings"]], "| isError:", r.get("isError", False))
send({"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name": "job_show", "arguments": {"job_id": "$(id)"}}})
r = recv(4)["result"]
print("job_show '$(id)' -> isError:", r.get("isError"), "|", r["content"][0]["text"][:80])
p.stdin.close()
p.wait(timeout=10)
