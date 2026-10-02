# Connecting clients

bifrost runs two ways. Pick one, then follow the section for your client.

| | Hosted (recommended) | Local |
|---|---|---|
| What runs | Cloud Run service `bifrost-mcp` | the `bifrost` binary on your machine |
| URL / command | `https://bifrost-mcp-125853442225.us-central1.run.app/mcp` | `bifrost mcp` (stdio) |
| Sign-in | Google, ucr.edu accounts listed in `users.yaml` | none: uses your own `gcloud` login and SSH |
| Install | nothing | `curl -fsSL https://raw.githubusercontent.com/UCR-Research-Computing/ursa-bifrost/main/scripts/get.sh \| sh` |
| Works from | any MCP client, any machine | your machine only |

The hosted server speaks **Streamable HTTP** at `/mcp` and does **OAuth 2.1** with
dynamic client registration (RFC 7591), PKCE and discovery (RFC 9728), so any client
that supports remote MCP servers signs you in by itself: it opens a browser, you pick
your ucr.edu Google account, done. Nothing to copy, no API keys.

> Below, `URL` means `https://bifrost-mcp-125853442225.us-central1.run.app/mcp`.
> Every command here was run against the live server on 2026-10-02.

## Contents

- [Hermes Agent](#hermes-agent)
- [Claude Code](#claude-code)
- [Claude Desktop and claude.ai](#claude-desktop-and-claudeai)
- [OpenAI Codex CLI](#openai-codex-cli)
- [OpenCode](#opencode)
- [Gemini CLI](#gemini-cli)
- [OpenAI API (Responses API)](#openai-api-responses-api)
- [Scripts (Python, curl)](#scripts-python-curl)
- [Any other client](#any-other-client)
- [Troubleshooting](#troubleshooting)

---

## Hermes Agent

```bash
hermes mcp add ursa --url https://bifrost-mcp-125853442225.us-central1.run.app/mcp --auth oauth
hermes mcp test ursa
```

`add` opens the browser to sign in and asks which tools to enable. Tokens are kept in
`~/.hermes/mcp-tokens/` and refresh on their own. Start a new session to get the tools
(`mcp_ursa_cluster_status`, ...).

Approval prompts: Hermes asks before every tool on an untrusted server. bifrost's
read tools are marked `readOnlyHint: true` and its act tools need a second confirm call
anyway, so you can skip the prompts with:

```bash
hermes config set mcp_servers.ursa.trust full
```

Local instead: `hermes mcp add ursa --command bifrost --args mcp`.

## Claude Code

```bash
claude mcp add --transport http ursa https://bifrost-mcp-125853442225.us-central1.run.app/mcp
```

Then, inside Claude Code, run `/mcp`, pick `ursa`, choose **Authenticate**, and sign in.
Add `-s user` to make it available in every project (default: this project only).

Local instead: `claude mcp add ursa -- bifrost mcp`.

## Claude Desktop and claude.ai

Settings -> Connectors -> **Add custom connector**:

- Name: `Ursa Major`
- URL: `https://bifrost-mcp-125853442225.us-central1.run.app/mcp`

Claude opens the sign-in page; pick your ucr.edu account. On a Team or Enterprise plan
an owner may need to allow custom connectors first.

Local instead (Desktop only), in `claude_desktop_config.json`:

```json
{ "mcpServers": { "ursa": { "command": "bifrost", "args": ["mcp"] } } }
```

## OpenAI Codex CLI

```bash
codex mcp add ursa --url https://bifrost-mcp-125853442225.us-central1.run.app/mcp
```

Codex (0.160+) sees that the server supports OAuth and opens the sign-in right away.
To sign in again later: `codex mcp login ursa` (`--no-browser` prints the URL instead).
Check with `codex mcp list`.

Or in `~/.codex/config.toml`:

```toml
[mcp_servers.ursa]
url = "https://bifrost-mcp-125853442225.us-central1.run.app/mcp"
```

then `codex mcp login ursa`. Local instead: `codex mcp add ursa -- bifrost mcp`.
Older Codex releases (the 0.1.x TypeScript CLI) have no `mcp` command; update with
`npm install -g @openai/codex`.

## OpenCode

```bash
opencode mcp add ursa --url https://bifrost-mcp-125853442225.us-central1.run.app/mcp
opencode mcp auth ursa
```

That writes `~/.config/opencode/opencode.jsonc`:

```jsonc
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "ursa": { "type": "remote", "url": "https://bifrost-mcp-125853442225.us-central1.run.app/mcp" }
  }
}
```

OpenCode registers itself with the server and stores tokens in
`~/.local/share/opencode/mcp-auth.json`. Local instead:
`"ursa": { "type": "local", "command": ["bifrost", "mcp"] }`.

## Gemini CLI

```bash
gemini mcp add -s user -t http ursa https://bifrost-mcp-125853442225.us-central1.run.app/mcp
```

Start `gemini`, then run `/mcp auth ursa` to sign in. Settings land in
`~/.gemini/settings.json`:

```json
{ "mcpServers": { "ursa": { "url": "https://bifrost-mcp-125853442225.us-central1.run.app/mcp", "type": "http" } } }
```

Local instead: `gemini mcp add -s user ursa bifrost mcp`.

## OpenAI API (Responses API)

The Responses API can call a remote MCP server itself. OpenAI's servers make the calls,
so you pass a bearer token you already have (get one with the script below:
`python3 examples/mcp_client.py login`, then read `access_token` from the token file;
it lasts an hour).

```python
import json, os
from openai import OpenAI

token = json.load(open(os.path.expanduser(
    "~/.config/mcp-client/bifrost-mcp-125853442225.us-central1.run.app.json")))["access_token"]

client = OpenAI()
resp = client.responses.create(
    model="gpt-5",
    tools=[{
        "type": "mcp",
        "server_label": "ursa",
        "server_url": "https://bifrost-mcp-125853442225.us-central1.run.app/mcp",
        "authorization": token,
        "require_approval": "never",
        "allowed_tools": ["cluster_status", "partitions", "job_explain", "script_check"],
    }],
    input="Is anything running on Ursa Major right now, and what does it cost per hour?",
)
print(resp.output_text)
```

Keep `allowed_tools` to read tools when `require_approval` is `never`. The act tools
(`job_submit`, `job_cancel`, ...) still need their confirm call, but a model running
unattended should not be the one making it.

## Scripts (Python, curl)

[`examples/mcp_client.py`](../examples/mcp_client.py) is a complete client in one file,
standard library only:

```bash
python3 examples/mcp_client.py login            # browser sign-in, tokens saved 0600
python3 examples/mcp_client.py tools
python3 examples/mcp_client.py call cluster_status
python3 examples/mcp_client.py call job_explain '{"job_id": "236"}'
python3 examples/mcp_client.py call script_check "$(jq -n --rawfile s job.sbatch '{script: $s}')"
```

The server is stateless, so after login a tool call is one HTTP POST:

```bash
TOKEN=$(jq -r .access_token ~/.config/mcp-client/bifrost-mcp-125853442225.us-central1.run.app.json)
curl -s https://bifrost-mcp-125853442225.us-central1.run.app/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cluster_status","arguments":{}}}'
```

With the official Python SDK (`pip install mcp`):

```python
import asyncio
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client

async def main(token: str):
    async with streamablehttp_client(
        "https://bifrost-mcp-125853442225.us-central1.run.app/mcp",
        headers={"Authorization": f"Bearer {token}"},
    ) as (read, write, _):
        async with ClientSession(read, write) as s:
            await s.initialize()
            r = await s.call_tool("partitions", {})
            print(r.content[0].text)
```

For scripts with no browser, the local binary is simpler: `bifrost status --json`,
`bifrost job explain 236 --json`. Every CLI command takes `--json`.

## Any other client

Point it at `URL` with transport "Streamable HTTP" (sometimes called "http"; not "SSE")
and let it do OAuth. Discovery starts from the `401` on `/mcp`, whose
`WWW-Authenticate` header points to `/.well-known/oauth-protected-resource`. Endpoints:
`/authorize`, `/token`, `/register`, `/revoke`. Redirect URIs must be `https` or an
`http` loopback address (`127.0.0.1`, `localhost`, `[::1]`; any port).

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Sign-in page says your account is not allowed | Only ucr.edu accounts listed in the server's `users.yaml` get in. Ask Research Computing to add you. |
| `401` on every call after it worked | Access tokens last an hour and refresh tokens 30 days; refresh tokens rotate, so two clients sharing one token file break each other. Sign in again per client. |
| Client says "SSE" or "invalid transport" | Choose Streamable HTTP / `http`. bifrost does not serve SSE. |
| A tool you expected is missing | Tools follow your tier: R1 read, R2 staff, A1 submit/cancel. The tool list only shows what you may call. |
| `job_submit` returns a token, nothing runs | That is the design: show the plan, then call `job_submit_confirm` with the token after you approve. Tokens expire in 10 minutes. |
| Every read asks for approval (Hermes) | Hermes treats untrusted servers' tools as writes; see the Hermes section for `trust full`. |
