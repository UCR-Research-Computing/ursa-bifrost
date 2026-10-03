"""Sign-ins that survive a restart (ursa-agent 0.4.0).

Cloud Run stops the agent after it sits unused (min-instances 0), which used to
sign everyone out: web sessions lived only in memory. Each sign-in is now also
written to a small file under AGENT_DATA_DIR (a Cloud Storage bucket mounted at
/data on Cloud Run), sealed with a key derived from AGENT_SESSION_SECRET, so a
copy of the bucket alone is useless. The file name is a hash of the session id,
so the bucket does not reveal the browser cookie either.

What is kept: email, tiers, the bifrost access and refresh tokens, their expiry,
the OAuth client id. Not kept: chat history and pending approvals (ADK sessions
stay in memory); after a restart the person keeps their sign-in and starts a new
chat. Without AGENT_DATA_DIR (tests, local runs) nothing is written.
"""

from __future__ import annotations

import base64
import hashlib
import json
import logging
import os
import time
from pathlib import Path
from typing import Any

from cryptography.fernet import Fernet, InvalidToken

log = logging.getLogger("ursa_agent")

# what a stored sign-in may contain; anything else is never written
FIELDS = ("email", "tiers", "token", "refresh", "exp", "client_id", "created")
MAX_AGE_S = 12 * 3600  # same as the session cookie


class SessionStore:
    def __init__(self, directory: str | os.PathLike | None, secret: str, max_age_s: int = MAX_AGE_S):
        self.dir = Path(directory) / "sessions" if directory else None
        key = base64.urlsafe_b64encode(hashlib.sha256(("ursa-agent-store|" + secret).encode()).digest())
        self.f = Fernet(key)
        self.max_age_s = max_age_s
        if self.dir:
            self.dir.mkdir(parents=True, exist_ok=True)

    @property
    def enabled(self) -> bool:
        return self.dir is not None

    def _path(self, sid: str) -> Path:
        assert self.dir is not None
        return self.dir / (hashlib.sha256(sid.encode()).hexdigest() + ".bin")

    def save(self, sid: str, w: dict[str, Any]) -> None:
        if not self.dir:
            return
        rec = {k: w[k] for k in FIELDS if k in w}
        rec.setdefault("created", time.time())
        p = self._path(sid)
        tmp = p.with_suffix(".tmp")
        try:
            tmp.write_bytes(self.f.encrypt(json.dumps(rec).encode()))
            os.replace(tmp, p)
        except OSError as e:  # the store is a convenience: never fail a request over it
            log.warning("session store write failed: %s", e)

    def load(self, sid: str) -> dict[str, Any] | None:
        if not self.dir:
            return None
        p = self._path(sid)
        try:
            raw = p.read_bytes()
        except OSError:
            return None
        try:
            rec = json.loads(self.f.decrypt(raw))
        except (InvalidToken, ValueError):
            log.warning("session store: unreadable record, removed")
            self.delete(sid)
            return None
        if time.time() - float(rec.get("created", 0)) > self.max_age_s:
            self.delete(sid)
            return None
        return {k: rec[k] for k in FIELDS if k in rec}

    def delete(self, sid: str) -> None:
        if not self.dir:
            return
        try:
            self._path(sid).unlink()
        except OSError:
            pass

    def sweep(self) -> int:
        """Remove records older than the cookie lifetime (at start-up)."""
        if not self.dir:
            return 0
        n, cutoff = 0, time.time() - self.max_age_s
        for p in self.dir.glob("*.bin"):
            try:
                if p.stat().st_mtime < cutoff:
                    p.unlink()
                    n += 1
            except OSError:
                pass
        return n

    # the OAuth client registration, so a restart does not register a new client
    def load_client(self, base_url: str) -> str | None:
        if not self.dir:
            return None
        try:
            d = json.loads(self.f.decrypt((self.dir.parent / "client.bin").read_bytes()))
        except (OSError, InvalidToken, ValueError):
            return None
        return d.get("id") if d.get("base_url") == base_url else None

    def save_client(self, base_url: str, client_id: str) -> None:
        if not self.dir:
            return
        p = self.dir.parent / "client.bin"
        try:
            p.with_suffix(".tmp").write_bytes(
                self.f.encrypt(json.dumps({"id": client_id, "base_url": base_url}).encode())
            )
            os.replace(p.with_suffix(".tmp"), p)
        except OSError as e:
            log.warning("client store write failed: %s", e)
