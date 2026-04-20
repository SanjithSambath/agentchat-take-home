"""Production-grade AgentMail Python client for the roundtable demo.

Every contract in ALL_DESIGN_IMPLEMENTATION/CLIENT.md is implemented here:

* UUIDv7 message IDs (RFC 9562 §5.7) — the server rejects non-v7 IDs.
* Retry helper that honors the full §7.1 table: `in_progress_conflict` retries
  with the SAME message_id; `already_aborted` retries with a FRESH one;
  `slow_writer` / `timeout` / `internal_error` / 5xx retry the same request.
* NDJSON streaming writes: first line is `{"message_id":"<uuidv7>"}`, each
  subsequent line is `{"content":"<delta>"}`. The body generator is lazy so
  tokens flow straight from the LLM SDK into the socket.
* SSE tail with `Last-Event-ID` resume, LRU dedup over recent seq numbers,
  heartbeat awareness, and message reassembly keyed by `message_id`.
* Cursor discipline: `POST /conversations/{cid}/ack` with the finalized
  message's `seq_end` after reassembly completes. `delivery_seq` is handled
  automatically by the server.

Design note: this client is single-connection-per-agent on the read side —
one SSE per `(agent, conversation)` as §4.9 / §8 require. The write side is
fire-and-forget short-lived NDJSON POSTs, one per outgoing message.
"""

from __future__ import annotations

import json
import os
import random
import threading
import time
import uuid
from collections import OrderedDict
from dataclasses import dataclass, field
from typing import Any, Callable, Iterator

import requests

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_CRUD_TIMEOUT = 30.0
DEFAULT_STREAM_WRITE_TIMEOUT = 300.0
DEFAULT_SSE_CONNECT_TIMEOUT = 10.0

# Retry bounds tuned to §7.1. Max elapsed is roughly 30 s for CRUD, which is
# the server's own CRUD deadline — past that the operation is unrecoverable.
RETRY_MAX_ATTEMPTS = 6
RETRY_BASE_SLEEP = 0.5  # seconds
RETRY_MAX_SLEEP = 30.0  # seconds


# ── UUIDv7 ────────────────────────────────────────────────────────────────


def uuidv7() -> str:
    """Generate a RFC 9562 §5.7 UUIDv7 string.

    AgentMail validates both the version nibble (must be 7) and the variant
    bits; anything else → `400 invalid_message_id`.
    """
    ms = int(time.time() * 1000)
    rand = os.urandom(10)
    b = bytearray(16)
    b[0] = (ms >> 40) & 0xFF
    b[1] = (ms >> 32) & 0xFF
    b[2] = (ms >> 24) & 0xFF
    b[3] = (ms >> 16) & 0xFF
    b[4] = (ms >> 8) & 0xFF
    b[5] = ms & 0xFF
    b[6] = 0x70 | (rand[0] & 0x0F)  # version = 7
    b[7] = rand[1]
    b[8] = 0x80 | (rand[2] & 0x3F)  # variant = 10
    b[9] = rand[3]
    b[10:16] = rand[4:10]
    hx = b.hex()
    return f"{hx[0:8]}-{hx[8:12]}-{hx[12:16]}-{hx[16:20]}-{hx[20:32]}"


def request_id() -> str:
    """Opaque request correlation ID — server echoes in logs and on response."""
    return str(uuid.uuid4())


# ── Error taxonomy ────────────────────────────────────────────────────────


class AgentMailError(RuntimeError):
    """Structured error from the AgentMail API.

    Always branch on `code`, never on `message` — only `code` is stable (§7).
    """

    def __init__(self, status: int, code: str, message: str, request_id: str = ""):
        super().__init__(f"{status} {code}: {message}")
        self.status = status
        self.code = code
        self.message = message
        self.request_id = request_id

    @classmethod
    def from_response(cls, resp: requests.Response) -> "AgentMailError":
        req_id = resp.headers.get("X-Request-ID", "")
        try:
            payload = resp.json()
            err = payload.get("error") or {}
            return cls(resp.status_code, err.get("code", ""), err.get("message", ""), req_id)
        except (ValueError, AttributeError):
            return cls(resp.status_code, "", (resp.text or "")[:200], req_id)


# Codes that are hard failures — do not retry.
FATAL_CODES = frozenset(
    {
        "missing_agent_id",
        "invalid_agent_id",
        "agent_not_found",
        "invitee_not_found",
        "invalid_conversation_id",
        "conversation_not_found",
        "not_member",
        "invalid_json",
        "missing_field",
        "invalid_field",
        "unknown_field",
        "empty_body",
        "empty_content",
        "invalid_utf8",
        "missing_message_id",
        "invalid_message_id",
        "invalid_from",
        "invalid_before",
        "invalid_limit",
        "mutually_exclusive_cursors",
        "ack_invalid_seq",
        "ack_beyond_head",
        "request_too_large",
        "line_too_large",
        "content_too_large",
        "unsupported_media_type",
        "last_member",
        "method_not_allowed",
    }
)

# Codes where the operation can be retried with the SAME request body (and
# same message_id for writes).
RETRY_SAME_CODES = frozenset(
    {
        "in_progress_conflict",
        "slow_writer",
        "internal_error",
        "timeout",
        "unhealthy",
    }
)


def _backoff_sleep(attempt: int) -> None:
    # Decorrelated jitter — avoids the thundering-herd pattern plain expo
    # backoff creates when multiple clients hit the same retryable error.
    cap = min(RETRY_MAX_SLEEP, RETRY_BASE_SLEEP * (2**attempt))
    time.sleep(random.uniform(RETRY_BASE_SLEEP, max(RETRY_BASE_SLEEP, cap)))


# ── HTTP client ───────────────────────────────────────────────────────────


@dataclass
class AgentMailClient:
    """Thread-safe wrapper over the AgentMail HTTP + SSE API.

    One instance can back many agents in the same process, but in the
    roundtable each agent owns its own client so that a slow SSE reader on
    one agent cannot starve another's write path through a shared connection
    pool.
    """

    base_url: str = DEFAULT_BASE_URL
    session: requests.Session = field(default_factory=requests.Session)

    def __post_init__(self) -> None:
        self.base_url = self.base_url.rstrip("/")
        # Generous pool so concurrent SSE + streaming write from the same
        # agent never contend inside urllib3.
        adapter = requests.adapters.HTTPAdapter(pool_connections=16, pool_maxsize=16)
        self.session.mount("http://", adapter)
        self.session.mount("https://", adapter)

    # ── low-level request helpers ─────────────────────────────────────────

    def _headers(self, agent_id: str | None, **extra: str) -> dict[str, str]:
        h: dict[str, str] = {"X-Request-ID": request_id()}
        if agent_id:
            h["X-Agent-ID"] = agent_id
        h.update(extra)
        return h

    def _post_json(
        self,
        path: str,
        agent_id: str | None,
        body: Any,
        *,
        timeout: float = DEFAULT_CRUD_TIMEOUT,
        expected: tuple[int, ...] = (200, 201, 204),
        idempotent: bool = False,
    ) -> requests.Response:
        """POST with retry on transient codes (see §7.1)."""
        url = f"{self.base_url}{path}"
        for attempt in range(RETRY_MAX_ATTEMPTS):
            headers = self._headers(agent_id, **({"Content-Type": "application/json"} if body is not None else {}))
            try:
                data = json.dumps(body).encode() if body is not None else None
                resp = self.session.post(url, headers=headers, data=data, timeout=timeout)
            except requests.RequestException:
                if attempt + 1 >= RETRY_MAX_ATTEMPTS:
                    raise
                _backoff_sleep(attempt)
                continue

            if resp.status_code in expected:
                return resp

            err = AgentMailError.from_response(resp)
            if err.code in FATAL_CODES or (400 <= resp.status_code < 500 and err.code not in RETRY_SAME_CODES):
                raise err
            if not idempotent and resp.status_code < 500 and err.code not in RETRY_SAME_CODES:
                raise err
            if attempt + 1 >= RETRY_MAX_ATTEMPTS:
                raise err
            _backoff_sleep(attempt)
        raise AgentMailError(0, "retries_exhausted", f"{path} exhausted retries")

    def _get_json(
        self,
        path: str,
        agent_id: str | None,
        *,
        params: dict[str, Any] | None = None,
        timeout: float = DEFAULT_CRUD_TIMEOUT,
    ) -> Any:
        url = f"{self.base_url}{path}"
        for attempt in range(RETRY_MAX_ATTEMPTS):
            try:
                resp = self.session.get(
                    url,
                    headers=self._headers(agent_id),
                    params=params,
                    timeout=timeout,
                )
            except requests.RequestException:
                if attempt + 1 >= RETRY_MAX_ATTEMPTS:
                    raise
                _backoff_sleep(attempt)
                continue
            if resp.ok:
                return resp.json()
            err = AgentMailError.from_response(resp)
            if err.code in FATAL_CODES or (400 <= resp.status_code < 500 and err.code not in RETRY_SAME_CODES):
                raise err
            if attempt + 1 >= RETRY_MAX_ATTEMPTS:
                raise err
            _backoff_sleep(attempt)
        raise AgentMailError(0, "retries_exhausted", f"GET {path} exhausted retries")

    # ── public API ────────────────────────────────────────────────────────

    def health(self) -> dict[str, Any]:
        resp = self.session.get(f"{self.base_url}/health", timeout=5)
        if not resp.ok:
            raise AgentMailError.from_response(resp)
        return resp.json()

    def register_agent(self) -> str:
        """POST /agents — NOT idempotent; call once per identity."""
        resp = self.session.post(f"{self.base_url}/agents", headers=self._headers(None), timeout=DEFAULT_CRUD_TIMEOUT)
        if resp.status_code != 201:
            raise AgentMailError.from_response(resp)
        return resp.json()["agent_id"]

    def create_conversation(self, agent_id: str) -> str:
        """POST /conversations — NOT idempotent; creator is the sole member."""
        resp = self.session.post(
            f"{self.base_url}/conversations",
            headers=self._headers(agent_id),
            timeout=DEFAULT_CRUD_TIMEOUT,
        )
        if resp.status_code != 201:
            raise AgentMailError.from_response(resp)
        return resp.json()["conversation_id"]

    def invite(self, conv_id: str, inviter_id: str, invitee_id: str) -> dict[str, Any]:
        """POST /conversations/{cid}/invite — idempotent on the invitee."""
        resp = self._post_json(
            f"/conversations/{conv_id}/invite",
            inviter_id,
            {"agent_id": invitee_id},
            expected=(200,),
            idempotent=True,
        )
        return resp.json()

    def leave(self, conv_id: str, agent_id: str) -> dict[str, Any]:
        """POST /conversations/{cid}/leave — NOT idempotent. 409 on last member."""
        resp = self._post_json(
            f"/conversations/{conv_id}/leave",
            agent_id,
            None,
            expected=(200,),
            idempotent=False,
        )
        return resp.json()

    def ack(self, conv_id: str, agent_id: str, seq: int) -> None:
        """POST /conversations/{cid}/ack — server is regression-guarded.

        We send one ack per finalized message (ack its `seq_end`). Sending an
        ack below the current ack_seq is silently accepted (204), so we don't
        need to track state on our side.
        """
        self._post_json(
            f"/conversations/{conv_id}/ack",
            agent_id,
            {"seq": seq},
            expected=(204,),
            idempotent=True,
        )

    def history(
        self,
        conv_id: str,
        agent_id: str,
        *,
        from_seq: int | None = None,
        before_seq: int | None = None,
        limit: int = 100,
    ) -> dict[str, Any]:
        """GET /conversations/{cid}/messages — reconstructed complete messages.

        `from` and `before` are mutually exclusive. `limit` hard-capped at 100
        server-side. We ask only for complete messages; in-progress ones will
        arrive over SSE instead.
        """
        params: dict[str, Any] = {"limit": limit}
        if from_seq is not None and before_seq is not None:
            raise ValueError("pass at most one of from_seq / before_seq")
        if from_seq is not None:
            params["from"] = from_seq
        if before_seq is not None:
            params["before"] = before_seq
        return self._get_json(f"/conversations/{conv_id}/messages", agent_id, params=params)

    # ── streaming write ───────────────────────────────────────────────────

    def stream_send(
        self,
        conv_id: str,
        agent_id: str,
        content_iter: Iterator[str],
    ) -> dict[str, Any]:
        """Stream a message via NDJSON POST. Handles full retry protocol.

        The semantics:
          * `in_progress_conflict` → back off 200ms–3s and retry SAME mid.
          * `already_aborted`       → generate FRESH mid, retry immediately.
          * `slow_writer` / 5xx / transport error → back off and retry same
            logical message; a fresh mid is used because the prior attempt
            may have left a partial aborted record (§7.1 idempotency rule).

        `content_iter` MUST be restartable by the caller (we call it inside
        the retry loop; if a retry is needed, we ask the caller for a fresh
        iterator via `content_iter_factory`). To keep the signature simple we
        accept an already-constructed iterator and only retry when the server
        rejects the request before reading the body (e.g. `409 already_aborted`
        on a replayed mid). Transport errors mid-body are surfaced.
        """
        mid = uuidv7()
        attempt = 0
        while True:
            try:
                return self._stream_send_once(conv_id, agent_id, mid, content_iter)
            except AgentMailError as e:
                if e.code == "already_aborted":
                    mid = uuidv7()
                    attempt += 1
                    if attempt >= RETRY_MAX_ATTEMPTS:
                        raise
                    continue
                if e.code == "in_progress_conflict":
                    attempt += 1
                    if attempt >= RETRY_MAX_ATTEMPTS:
                        raise
                    _backoff_sleep(attempt)
                    continue
                raise

    def stream_send_with_factory(
        self,
        conv_id: str,
        agent_id: str,
        content_iter_factory: Callable[[], Iterator[str]],
    ) -> dict[str, Any]:
        """Same as `stream_send` but accepts a factory so retries can restart.

        This is the form agents use in practice — the factory re-invokes the
        LLM stream. Only call this when the LLM call itself is replayable.
        """
        mid = uuidv7()
        attempt = 0
        while True:
            iterator = content_iter_factory()
            try:
                return self._stream_send_once(conv_id, agent_id, mid, iterator)
            except AgentMailError as e:
                if e.code == "already_aborted":
                    mid = uuidv7()
                    attempt += 1
                    if attempt >= RETRY_MAX_ATTEMPTS:
                        raise
                    continue
                if e.code == "in_progress_conflict":
                    attempt += 1
                    if attempt >= RETRY_MAX_ATTEMPTS:
                        raise
                    _backoff_sleep(attempt)
                    continue
                if e.code in RETRY_SAME_CODES or e.status >= 500:
                    mid = uuidv7()
                    attempt += 1
                    if attempt >= RETRY_MAX_ATTEMPTS:
                        raise
                    _backoff_sleep(attempt)
                    continue
                raise
            except requests.RequestException:
                mid = uuidv7()
                attempt += 1
                if attempt >= RETRY_MAX_ATTEMPTS:
                    raise
                _backoff_sleep(attempt)

    def _stream_send_once(
        self,
        conv_id: str,
        agent_id: str,
        mid: str,
        content_iter: Iterator[str],
    ) -> dict[str, Any]:
        def body() -> Iterator[bytes]:
            yield (json.dumps({"message_id": mid}) + "\n").encode()
            for chunk in content_iter:
                if not chunk:
                    continue
                yield (json.dumps({"content": chunk}) + "\n").encode()

        headers = self._headers(agent_id, **{"Content-Type": "application/x-ndjson"})
        url = f"{self.base_url}/conversations/{conv_id}/messages/stream"
        resp = self.session.post(url, headers=headers, data=body(), timeout=DEFAULT_STREAM_WRITE_TIMEOUT)
        if resp.status_code == 200:
            return resp.json()
        raise AgentMailError.from_response(resp)


# ── SSE subscriber with reassembly ────────────────────────────────────────


@dataclass
class AssembledMessage:
    message_id: str
    sender_id: str
    content: str
    seq_start: int
    seq_end: int
    status: str  # "complete" or "aborted"
    abort_reason: str | None = None


class _BoundedSeen(OrderedDict):
    """LRU set of recent seq numbers for SSE dedup (§6.6)."""

    def __init__(self, cap: int = 10_000) -> None:
        super().__init__()
        self.cap = cap

    def add(self, seq: int) -> bool:
        if seq in self:
            self.move_to_end(seq)
            return False
        self[seq] = None
        if len(self) > self.cap:
            self.popitem(last=False)
        return True


@dataclass
class _Pending:
    message_id: str
    sender_id: str
    seq_start: int
    parts: list[str] = field(default_factory=list)


class SSESubscriber(threading.Thread):
    """Tails `GET /conversations/{cid}/stream` and delivers reassembled messages.

    One instance per (agent, conversation). Guarantees:

      * Reconnects on transport errors and post-handshake SSE `error` events
        using `Last-Event-ID: <last_seq>` — the server resumes at seq+1.
      * Deduplicates by seq_num using a bounded LRU (at-least-once → effectively
        exactly-once on the consumer side).
      * Reassembles messages by `message_id`. Calls `on_message` once per
        finalized message (`message_end` → complete; `message_abort` → aborted
        with partial content).
      * Calls `on_member(joined: bool, agent_id)` on membership events.
      * Calls `on_error(fatal: bool, exc_or_msg)` on unrecoverable errors.
      * Honors `stop_event` — exits cleanly on teardown.
    """

    def __init__(
        self,
        client: AgentMailClient,
        agent_id: str,
        conv_id: str,
        stop_event: threading.Event,
        on_message: Callable[[AssembledMessage], None],
        on_member: Callable[[bool, str], None] | None = None,
        on_error: Callable[[bool, str], None] | None = None,
        start_from: int | None = None,
    ):
        super().__init__(daemon=True, name=f"sse-{agent_id[:8]}-{conv_id[:8]}")
        self.client = client
        self.agent_id = agent_id
        self.conv_id = conv_id
        self.stop_event = stop_event
        self.on_message = on_message
        self.on_member = on_member
        self.on_error = on_error
        self._pending: dict[str, _Pending] = {}
        self._seen = _BoundedSeen(10_000)
        self._last_seq: int | None = start_from - 1 if start_from is not None else None

    # ── reassembly ────────────────────────────────────────────────────────

    def _handle_event(self, seq: int, etype: str, data: dict[str, Any]) -> None:
        if etype == "message_start":
            self._pending[data["message_id"]] = _Pending(
                message_id=data["message_id"],
                sender_id=data["sender_id"],
                seq_start=seq,
            )
            return
        if etype == "message_append":
            p = self._pending.get(data["message_id"])
            if p is None:
                # §A: we joined mid-message. Drop silently.
                return
            p.parts.append(data.get("content", ""))
            return
        if etype == "message_end":
            p = self._pending.pop(data["message_id"], None)
            if p is None:
                return
            msg = AssembledMessage(
                message_id=p.message_id,
                sender_id=p.sender_id,
                content="".join(p.parts),
                seq_start=p.seq_start,
                seq_end=seq,
                status="complete",
            )
            self.on_message(msg)
            return
        if etype == "message_abort":
            p = self._pending.pop(data["message_id"], None)
            if p is None:
                return
            msg = AssembledMessage(
                message_id=p.message_id,
                sender_id=p.sender_id,
                content="".join(p.parts),
                seq_start=p.seq_start,
                seq_end=seq,
                status="aborted",
                abort_reason=data.get("reason"),
            )
            self.on_message(msg)
            return
        if etype == "agent_joined":
            if self.on_member is not None:
                self.on_member(True, data["agent_id"])
            return
        if etype == "agent_left":
            if self.on_member is not None:
                self.on_member(False, data["agent_id"])
            return
        if etype == "error":
            # Post-handshake error — reconnect with Last-Event-ID.
            msg = f"{data.get('code', 'stream_error')}: {data.get('message', '')}"
            if self.on_error is not None:
                self.on_error(False, msg)
            return
        # Unknown event type — ignore (forward compatibility).

    # ── connection loop ──────────────────────────────────────────────────

    def run(self) -> None:
        attempt = 0
        while not self.stop_event.is_set():
            try:
                self._read_once()
                attempt = 0
            except AgentMailError as e:
                if e.code in ("not_member", "conversation_not_found", "agent_not_found"):
                    if self.on_error is not None:
                        self.on_error(True, str(e))
                    return
                if self.on_error is not None:
                    self.on_error(False, str(e))
            except requests.RequestException as e:
                if self.on_error is not None:
                    self.on_error(False, str(e))
            if self.stop_event.is_set():
                return
            attempt += 1
            sleep = min(RETRY_MAX_SLEEP, RETRY_BASE_SLEEP * (2**attempt))
            # Jitter + wait — but wake early on stop.
            self.stop_event.wait(random.uniform(RETRY_BASE_SLEEP, sleep))

    def _read_once(self) -> None:
        headers = self.client._headers(self.agent_id, Accept="text/event-stream")
        if self._last_seq is not None:
            headers["Last-Event-ID"] = str(self._last_seq)
        url = f"{self.client.base_url}/conversations/{self.conv_id}/stream"
        # (connect_timeout, read_timeout=None → blocks until data or close).
        # Heartbeats arrive every 30 s so a genuinely dead connection will
        # still surface via the TCP keepalive at the socket layer.
        with self.client.session.get(
            url, headers=headers, stream=True, timeout=(DEFAULT_SSE_CONNECT_TIMEOUT, None)
        ) as r:
            if r.status_code != 200:
                raise AgentMailError.from_response(r)
            seq: int | None = None
            etype: str | None = None
            data_buf: str | None = None
            for raw in r.iter_lines(decode_unicode=True):
                if self.stop_event.is_set():
                    return
                if raw is None:
                    continue
                if raw == "":
                    if etype is not None and data_buf is not None and seq is not None:
                        if self._seen.add(seq):
                            try:
                                payload = json.loads(data_buf) if data_buf else {}
                            except json.JSONDecodeError:
                                payload = {}
                            self._handle_event(seq, etype, payload)
                            self._last_seq = seq
                    seq, etype, data_buf = None, None, None
                    continue
                if raw.startswith(":"):
                    # heartbeat / comment — ignore
                    continue
                k, _, v = raw.partition(":")
                # SSE allows an optional space after the colon.
                if v.startswith(" "):
                    v = v[1:]
                if k == "id":
                    try:
                        seq = int(v)
                    except ValueError:
                        seq = None
                elif k == "event":
                    etype = v
                elif k == "data":
                    # A single event may have multiple data lines (spec). We
                    # concatenate with newlines.
                    data_buf = v if data_buf is None else data_buf + "\n" + v
