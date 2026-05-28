"""LongMemEval harness against a running Anamnesia stack.

Pipeline per question:
  1. mint a fresh user (`lme-<question_id>`) so memory does not leak across questions
  2. POST each haystack session to /v1/ingest with the matching haystack_date as occurred_at
  3. poll /v1/queue/pending until extract + embed queues drain (cap: --ingest-wait)
  4. POST the question to /v1/retrieve, take the top hits
  5. ask the configured LLM to answer using only those hits
  6. ask an LLM judge to score the answer against the gold answer

Outputs a JSONL trace + an aggregate accuracy breakdown by question_type.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
from collections import defaultdict
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

import httpx


_DOW = re.compile(r"\s*\([A-Za-z]{3}\)\s*")


def load_env_file(path: Path) -> None:
    """Read KEY=VALUE lines (Docker-style .env) into os.environ. Existing
    env vars win — pass --env-file but `export OPENAI_API_KEY=…` still
    takes precedence."""
    if not path.exists():
        raise FileNotFoundError(f"env file not found: {path}")
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, _, v = line.partition("=")
        k = k.strip()
        v = v.strip().strip('"').strip("'")
        if k and k not in os.environ:
            os.environ[k] = v


def to_rfc3339(stamp: str | None) -> str | None:
    """Coerce a dataset timestamp into RFC3339 UTC for /v1/ingest.
    Handles:
      LongMemEval — '2023/05/20 (Sat) 02:21'
      LoCoMo      — '1:56 pm on 8 May, 2023'"""
    if not stamp:
        return None
    cleaned = _DOW.sub(" ", stamp).strip()
    for fmt in (
        "%Y/%m/%d %H:%M",
        "%Y/%m/%d %H:%M:%S",
        "%I:%M %p on %d %B, %Y",
        "%I:%M %p on %d %B %Y",
        "%I:%M%p on %d %B, %Y",
    ):
        try:
            return datetime.strptime(cleaned, fmt).replace(tzinfo=timezone.utc).isoformat()
        except ValueError:
            continue
    return None


# ---------- Anamnesia client ------------------------------------------------


@dataclass
class Anamnesia:
    base_url: str
    token: str | None
    timeout: float = 60.0

    def _headers(self) -> dict[str, str]:
        h = {"Content-Type": "application/json"}
        if self.token:
            h["Authorization"] = f"Bearer {self.token}"
        return h

    def health(self) -> None:
        r = httpx.get(f"{self.base_url}/v1/health", timeout=self.timeout)
        r.raise_for_status()

    def ingest(
        self,
        *,
        user: str,
        content: str,
        occurred_at: str | None,
        kind: str = "longmemeval-session",
        external_ref: str | None = None,
    ) -> dict[str, Any]:
        body: dict[str, Any] = {
            "user": user,
            "kind": kind,
            "content": content,
        }
        if occurred_at:
            body["occurred_at"] = occurred_at
        if external_ref:
            body["external_ref"] = external_ref
        r = httpx.post(
            f"{self.base_url}/v1/ingest",
            json=body,
            headers=self._headers(),
            timeout=self.timeout,
        )
        if r.status_code >= 400:
            raise RuntimeError(
                f"ingest {r.status_code}: {r.text.strip()[:400]} "
                f"(body kind={body.get('kind')!r} "
                f"len(content)={len(body.get('content') or '')} "
                f"occurred_at={body.get('occurred_at')!r})"
            )
        return r.json()

    def experience(
        self,
        *,
        user: str,
        body: str,
        occurred_at: str | None,
        title: str | None = None,
        topic: str | None = None,
        participants: list[str] | None = None,
    ) -> dict[str, Any]:
        """Direct write to /v1/experience, bypassing the extractor. Used
        by the RAG baseline ingest mode — the body lands in memory verbatim
        and is embedded inline before this call returns."""
        body_req: dict[str, Any] = {"user": user, "body": body}
        if occurred_at:
            body_req["occurred_at"] = occurred_at
        if title:
            body_req["title"] = title
        if topic:
            body_req["topic"] = topic
        if participants:
            body_req["participants"] = participants
        r = httpx.post(
            f"{self.base_url}/v1/experience",
            json=body_req,
            headers=self._headers(),
            timeout=self.timeout,
        )
        if r.status_code >= 400:
            raise RuntimeError(
                f"experience {r.status_code}: {r.text.strip()[:400]} "
                f"(len(body)={len(body)} occurred_at={occurred_at!r})"
            )
        return r.json()

    def retrieve(self, *, user: str, prompt: str, k: int = 0) -> list[dict[str, Any]]:
        # only_raw=true: benchmarks need verbatim source evidence, not the
        # consolidator's thematic summaries that strip dates/quotes.
        body: dict[str, Any] = {"user": user, "prompt": prompt, "only_raw": True}
        if k > 0:
            body["k"] = k
        r = httpx.post(
            f"{self.base_url}/v1/retrieve",
            json=body,
            headers=self._headers(),
            timeout=self.timeout,
        )
        r.raise_for_status()
        return r.json().get("hits") or []

    def queue_pending(self, *, user: str) -> dict[str, int]:
        """Return current background-work counts for one user:
        {extract_pending: N, embed_pending: M}. Used to gate the benchmark
        on actual extraction/embedding completion instead of a fixed sleep."""
        r = httpx.get(
            f"{self.base_url}/v1/queue/pending",
            params={"user": user},
            headers=self._headers(),
            timeout=self.timeout,
        )
        r.raise_for_status()
        return r.json()

    def wait_for_queue_drain(
        self,
        *,
        user: str,
        timeout_s: float,
        poll_s: float = 2.0,
        on_progress=None,
    ) -> dict[str, int]:
        """Poll /v1/queue/pending until both counters hit 0 or `timeout_s`
        elapses. Returns the final counts. `on_progress(counts)` fires once
        per poll for callers that want to log progress."""
        deadline = time.monotonic() + timeout_s
        last: dict[str, int] = {"extract_pending": -1, "embed_pending": -1}
        while True:
            counts = self.queue_pending(user=user)
            if on_progress is not None:
                on_progress(counts)
            if counts.get("extract_pending", 0) <= 0 and counts.get("embed_pending", 0) <= 0:
                return counts
            last = counts
            if time.monotonic() >= deadline:
                return last
            time.sleep(poll_s)


# ---------- LLM wrappers ----------------------------------------------------


_RATE_LIMIT_TPM = int(os.environ.get("HARNESS_TPM", "25000"))  # ~80% of tier-1 30K
_rate_log: list[tuple[float, int]] = []  # (timestamp, tokens_charged)


def _throttle(estimated_tokens: int) -> None:
    """Token-bucket throttle against a 60s sliding window. Sleeps until the
    rolling sum of charged tokens in the last minute leaves room for this
    request. Conservative — uses our own estimate, not OpenAI's. Set
    HARNESS_TPM=0 to disable."""
    if _RATE_LIMIT_TPM <= 0:
        return
    while True:
        now = time.monotonic()
        cutoff = now - 60.0
        while _rate_log and _rate_log[0][0] < cutoff:
            _rate_log.pop(0)
        used = sum(t for _, t in _rate_log)
        if used + estimated_tokens <= _RATE_LIMIT_TPM:
            _rate_log.append((now, estimated_tokens))
            return
        # A single request can legitimately exceed the per-minute budget
        # (e.g. RAG mode dumps many raw session bodies into one prompt).
        # Don't deadlock — log it and let the request through. Either it
        # succeeds, or the provider rejects it on its own terms.
        if not _rate_log:
            _rate_log.append((now, estimated_tokens))
            return
        oldest = _rate_log[0][0]
        sleep_for = max(0.5, 60.0 - (now - oldest) + 0.5)
        time.sleep(sleep_for)


def call_llm(provider: str, model: str, system: str, user: str) -> str:
    estimated = max(500, (len(system) + len(user)) // 4 + 600)
    _throttle(estimated)
    if provider == "anthropic":
        import anthropic

        client = anthropic.Anthropic()
        msg = client.messages.create(
            model=model,
            max_tokens=1024,
            system=system,
            messages=[{"role": "user", "content": user}],
        )
        parts = [b.text for b in msg.content if getattr(b, "type", "") == "text"]
        return "".join(parts).strip()
    if provider == "openai":
        from openai import OpenAI

        client = OpenAI()
        resp = client.chat.completions.create(
            model=model,
            messages=[
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
            max_tokens=1024,
        )
        return (resp.choices[0].message.content or "").strip()
    if provider == "openrouter":
        from openai import OpenAI

        api_key = os.environ.get("OPENROUTER_API_KEY") or os.environ.get("OPENAI_API_KEY")
        if not api_key:
            raise RuntimeError("openrouter provider requires OPENROUTER_API_KEY")
        client = OpenAI(
            api_key=api_key,
            base_url="https://openrouter.ai/api/v1",
            default_headers={
                "HTTP-Referer": "https://github.com/flohs/anamnesia-open-source",
                "X-Title": "Anamnesia",
            },
        )
        resp = client.chat.completions.create(
            model=model,
            messages=[
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
            max_tokens=1024,
        )
        return (resp.choices[0].message.content or "").strip()
    raise ValueError(f"unknown provider: {provider}")


ANSWER_SYSTEM = (
    "You answer questions about a user using only the supplied memory snippets "
    "as evidence. Counting, summing, listing, and combining multiple snippets is "
    "expected when the question calls for it. Be concise — 1-2 sentences. If the "
    "snippets genuinely don't contain the information after honest reasoning, "
    "reply exactly: I don't know."
)

JUDGE_SYSTEM = (
    "You are a strict grader. Compare a model answer to a gold answer and decide "
    "if the model answer is semantically correct. Respond with a single JSON "
    'object: {"correct": true|false, "reason": "..."}. No prose outside the JSON.'
)

EXPAND_SYSTEM = (
    "You generate retrieval sub-queries for a memory system. Given a question, "
    "emit 3-5 short queries (each 2-6 words) that target distinct entities, "
    "aspects, or facets needed to answer it. Counting questions especially need "
    "queries for each individual item kind. Respond with a single JSON object: "
    '{"queries": ["...", "...", ...]}. No prose outside the JSON.'
)


def expand_query(question: str, *, provider: str, model: str) -> list[str]:
    """Ask the LLM to expand a question into retrieval sub-queries.
    Returns the original question first, then deduped sub-queries."""
    try:
        raw = call_llm(provider, model, EXPAND_SYSTEM, f"Question: {question}")
        data = json.loads(raw)
        subs = [s.strip() for s in data.get("queries", []) if isinstance(s, str) and s.strip()]
    except (json.JSONDecodeError, KeyError, ValueError, TypeError):
        subs = []
    out = [question]
    seen = {question.lower()}
    for s in subs:
        low = s.lower()
        if low not in seen:
            out.append(s)
            seen.add(low)
    return out


def multi_retrieve(
    anam: "Anamnesia",
    *,
    user: str,
    question: str,
    k_per_query: int,
    max_total: int,
    expand_provider: str,
    expand_model: str,
) -> list[dict[str, Any]]:
    """Fan out to /v1/retrieve once per expanded sub-query, union by hit
    id, return up to max_total. The original question is always one of
    the queries so single-query parity is preserved on degenerate cases."""
    queries = expand_query(question, provider=expand_provider, model=expand_model)
    seen: dict[str, dict[str, Any]] = {}
    for q in queries:
        for h in anam.retrieve(user=user, prompt=q, k=k_per_query):
            hid = (
                (h.get("experience") or {}).get("id")
                or (h.get("fact") or {}).get("id")
                or json.dumps(h, sort_keys=True)[:128]
            )
            if hid not in seen:
                seen[hid] = h
                if len(seen) >= max_total:
                    return list(seen.values())
    return list(seen.values())


def _date_only(rfc: str | None) -> str | None:
    """Pull YYYY-MM-DD out of an RFC3339 timestamp. Returns None if the
    string isn't parseable as a date."""
    if not rfc:
        return None
    try:
        return rfc[:10] if len(rfc) >= 10 and rfc[4] == "-" and rfc[7] == "-" else None
    except (IndexError, TypeError):
        return None


def render_hits(hits: list[dict[str, Any]]) -> str:
    if not hits:
        return "(no memory snippets retrieved)"
    lines = []
    for i, h in enumerate(hits, 1):
        # /v1/retrieve hits nest the payload by domain. Pull the
        # human-readable body from the right place per domain so the
        # answerer sees the actual content, not a JSON dump.
        exp = h.get("experience") or {}
        fact = h.get("fact") or {}
        if exp:
            parts = []
            if exp.get("title"):
                parts.append(str(exp["title"]))
            if exp.get("body"):
                parts.append(str(exp["body"]))
            body = " — ".join(parts) if parts else json.dumps(exp)
            # Anchor the snippet in time so the model can resolve
            # "yesterday" / "last week" against an absolute date.
            d = _date_only(exp.get("occurred_at"))
            if d:
                body = f"[date: {d}] {body}"
        elif fact:
            val = fact.get("value")
            if isinstance(val, dict) and "v" in val:
                val = val["v"]
            body = f"{fact.get('key', '')}: {val}"
            d = _date_only(fact.get("valid_from") or fact.get("ingested_at"))
            if d:
                body = f"[date: {d}] {body}"
        else:
            body = h.get("body") or h.get("text") or json.dumps(h)
        lines.append(f"[{i}] {body}")
    return "\n".join(lines)


def answer_question(
    *,
    provider: str,
    model: str,
    question: str,
    hits: list[dict[str, Any]],
    now_date: str | None = None,
) -> str:
    system = ANSWER_SYSTEM
    if now_date:
        system = f"{ANSWER_SYSTEM}\n\nFor temporal grounding, today's date is {now_date}. Resolve relative time expressions in the snippets against this date when answering."
    user = f"Memory snippets:\n{render_hits(hits)}\n\nQuestion: {question}"
    return call_llm(provider, model, system, user)


def judge_answer(
    *, provider: str, model: str, question: str, gold: str, predicted: str
) -> dict[str, Any]:
    user = (
        f"Question: {question}\n\nGold answer: {gold}\n\nModel answer: {predicted}"
    )
    raw = call_llm(provider, model, JUDGE_SYSTEM, user)
    try:
        return json.loads(raw)
    except json.JSONDecodeError:
        return {"correct": False, "reason": f"unparseable judge output: {raw!r}"}


# ---------- Dataset loading -------------------------------------------------


def load_dataset(path: Path) -> list[dict[str, Any]]:
    with path.open() as f:
        return json.load(f)


def session_to_text(session: list[dict[str, Any]]) -> str:
    turns = []
    for t in session:
        role = t.get("role", "user")
        content = t.get("content", "")
        turns.append(f"{role}: {content}")
    return "\n".join(turns)


# ---------- Runner ----------------------------------------------------------


def run_question(
    q: dict[str, Any],
    *,
    anam: Anamnesia,
    user_prefix: str,
    ingest_wait: float,
    retrieve_k: int,
    multi_query: bool,
    multi_query_total: int,
    skip_ingest: bool,
    ingest_mode: str,
    generate_provider: str,
    generate_model: str,
    judge_provider: str,
    judge_model: str,
) -> dict[str, Any]:
    # `user_id` on the question (LoCoMo path) overrides the per-question
    # user-prefix scheme; LongMemEval still uses lme-<question_id>.
    user = q.get("user_id") or f"{user_prefix}-{q['question_id']}"
    sessions: list[list[dict[str, Any]]] = q.get("haystack_sessions", [])
    dates: list[str] = q.get("haystack_dates", [])
    ids: list[str] = q.get("haystack_session_ids", [])

    if not skip_ingest:
        for i, session in enumerate(sessions):
            content = session_to_text(session)
            if not content.strip():
                continue
            occ = to_rfc3339(dates[i] if i < len(dates) else None)
            ext = ids[i] if i < len(ids) else None
            if ingest_mode == "raw":
                # Bypass the extractor entirely: each session lands as one
                # experience row, embedded inline. RAG baseline.
                anam.experience(
                    user=user,
                    body=content,
                    occurred_at=occ,
                    title=ext,
                )
            else:
                anam.ingest(
                    user=user,
                    content=content,
                    occurred_at=occ,
                    external_ref=ext,
                )

        # Queue poll is only meaningful for the async-extract path. Raw
        # mode is fully synchronous — retrieval is warm the moment the
        # last /v1/experience POST returns.
        if ingest_mode != "raw" and ingest_wait > 0:
            final = anam.wait_for_queue_drain(user=user, timeout_s=ingest_wait)
            if final.get("extract_pending", 0) > 0 or final.get("embed_pending", 0) > 0:
                print(
                    f"  queue did not drain within {ingest_wait:.0f}s: {final}",
                    file=sys.stderr,
                )

    if multi_query:
        hits = multi_retrieve(
            anam,
            user=user,
            question=q["question"],
            k_per_query=retrieve_k,
            max_total=multi_query_total,
            expand_provider=generate_provider,
            expand_model=generate_model,
        )
    else:
        hits = anam.retrieve(user=user, prompt=q["question"], k=retrieve_k)
    # Resolve a "today's date" anchor so the answerer can ground relative
    # time language in the retrieved snippets.
    #   LongMemEval: each q carries question_date.
    #   LoCoMo:      the question is implicitly asked after the last
    #                session — use its date.
    now_date = _date_only(to_rfc3339(q.get("question_date")))
    if not now_date and dates:
        for d in reversed(dates):
            now_date = _date_only(to_rfc3339(d))
            if now_date:
                break
    predicted = answer_question(
        provider=generate_provider,
        model=generate_model,
        question=q["question"],
        hits=hits,
        now_date=now_date,
    )
    verdict = judge_answer(
        provider=judge_provider,
        model=judge_model,
        question=q["question"],
        gold=q["answer"],
        predicted=predicted,
    )
    return {
        "question_id": q["question_id"],
        "question_type": q.get("question_type", "unknown"),
        "question": q["question"],
        "gold": q["answer"],
        "predicted": predicted,
        "hits": hits,
        "correct": bool(verdict.get("correct")),
        "judge_reason": verdict.get("reason"),
    }


def iter_questions(
    dataset: list[dict[str, Any]], limit: int | None, types: list[str] | None
) -> Iterable[dict[str, Any]]:
    n = 0
    for q in dataset:
        if types and q.get("question_type") not in types:
            continue
        yield q
        n += 1
        if limit and n >= limit:
            return


# LoCoMo's QA "category" int → readable type label, matching the
# breakdown in the LoCoMo paper. Used so the per-question-type accuracy
# tally is meaningful.
_LOCOMO_CATEGORY_LABELS: dict[int, str] = {
    1: "single-hop",
    2: "multi-hop",
    3: "open-domain",
    4: "temporal-reasoning",
    5: "adversarial",
}


def iter_questions_locomo(
    dataset: list[dict[str, Any]],
    limit: int | None,
    types: list[str] | None,
    user_prefix: str = "locomo",
) -> Iterable[dict[str, Any]]:
    """LoCoMo: each top-level entry is one multi-session conversation
    with ~200 QA pairs sharing one haystack. Yields one item per
    (sample, qa); items in the same sample share `user_id`, so the
    main loop's `ingested_users` set deduplicates the ingest pass.
    `user_prefix` lets a caller scope a run to a fresh namespace
    (e.g. "locomo-extract" vs "locomo-raw") so different ingest modes
    don't reuse each other's experiences."""
    n = 0
    for sample in dataset:
        sample_id = sample.get("sample_id") or f"sample-{n}"
        conv = sample.get("conversation") or {}
        sessions: list[list[dict[str, Any]]] = []
        dates: list[str] = []
        session_ids: list[str] = []
        i = 1
        while True:
            sk = f"session_{i}"
            if sk not in conv:
                break
            turns = conv.get(sk) or []
            normalised = [
                {"role": t.get("speaker") or "user", "content": t.get("text") or ""}
                for t in turns
            ]
            sessions.append(normalised)
            dates.append(conv.get(f"{sk}_date_time") or "")
            session_ids.append(f"{sample_id}:S{i}")
            i += 1
        user_id = f"{user_prefix}-{sample_id}"
        for qa in sample.get("qa") or []:
            cat = qa.get("category")
            qtype = _LOCOMO_CATEGORY_LABELS.get(cat, f"category-{cat}")
            if types and qtype not in types:
                continue
            answer = qa.get("answer")
            yield {
                "question_id": f"{sample_id}#{n}",
                "question_type": qtype,
                "question": qa.get("question") or "",
                "answer": str(answer) if answer is not None else "",
                "haystack_sessions": sessions,
                "haystack_dates": dates,
                "haystack_session_ids": session_ids,
                "user_id": user_id,
            }
            n += 1
            if limit and n >= limit:
                return


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description="LongMemEval harness for Anamnesia")
    ap.add_argument(
        "--dataset",
        required=True,
        type=Path,
        help="path to a LongMemEval JSON file (longmemeval_s/m/oracle.json)",
    )
    ap.add_argument(
        "--env-file",
        type=Path,
        default=None,
        help="path to a Docker-style .env file with OPENAI_API_KEY / ANTHROPIC_API_KEY / ANAMNESIA_SERVER_TOKEN (loaded before client init)",
    )
    ap.add_argument(
        "--base-url",
        default=os.environ.get("ANAMNESIA_BASE_URL", "http://localhost:8181"),
    )
    ap.add_argument(
        "--token",
        default=os.environ.get("ANAMNESIA_SERVER_TOKEN") or None,
    )
    ap.add_argument(
        "--user-prefix",
        default="lme",
        help="per-question user is f'{prefix}-{question_id}'",
    )
    ap.add_argument(
        "--ingest-wait",
        type=float,
        default=600.0,
        help="hard upper bound, in seconds, on how long to wait for the extract + embed queues to drain after ingesting all sessions for a question. The harness polls /v1/queue/pending every 2s and proceeds the instant both queues hit 0; this flag only caps the worst case.",
    )
    ap.add_argument(
        "--retrieve-k",
        type=int,
        default=20,
        help="how many hits to fetch from /v1/retrieve per (sub-)query (server default is 10)",
    )
    ap.add_argument(
        "--multi-query",
        action=argparse.BooleanOptionalAction,
        default=False,
        help="expand each question into LLM-generated sub-queries and union the hits. Off by default — empirically adds noise that hurts the answerer more often than it helps at small sample sizes. Useful for counting/aggregation questions, harmful for direct-fact questions.",
    )
    ap.add_argument(
        "--multi-query-total",
        type=int,
        default=40,
        help="cap on total unique hits passed to the answerer when --multi-query is on",
    )
    ap.add_argument(
        "--skip-ingest",
        action="store_true",
        help="skip POSTing to /v1/ingest. Reuses whatever facts already exist for each per-question user — enables cheap cached re-runs of retrieve/answer/judge after a one-time ingestion pass.",
    )
    ap.add_argument(
        "--ingest-mode",
        choices=["extract", "raw"],
        default="extract",
        help="extract = POST /v1/ingest, run LLM extractor, retrieve facts/experiences (Anamnesia's full pipeline). raw = POST /v1/experience for each session, embed inline, no extractor, no queue poll — gives you a pure RAG baseline for comparison.",
    )
    ap.add_argument(
        "--dataset-format",
        choices=["longmemeval", "locomo"],
        default="longmemeval",
        help="longmemeval = the LongMemEval JSON (one question per top-level entry, with its own haystack). locomo = snap-research/locomo10.json (each sample is a multi-session conversation between two personas with many QA pairs sharing one haystack; the harness ingests the conversation once per sample and asks every QA against the shared memory).",
    )
    ap.add_argument("--limit", type=int, default=None)
    ap.add_argument(
        "--types",
        nargs="*",
        default=None,
        help="restrict to question_type values (e.g. temporal-reasoning knowledge-update)",
    )
    ap.add_argument(
        "--generate-provider",
        choices=["anthropic", "openai", "openrouter"],
        default="anthropic",
    )
    ap.add_argument("--generate-model", default="claude-sonnet-4-6")
    ap.add_argument(
        "--judge-provider",
        choices=["anthropic", "openai", "openrouter"],
        default="openai",
    )
    ap.add_argument("--judge-model", default="gpt-4o-mini")
    ap.add_argument(
        "--out", type=Path, required=True, help="JSONL output path for per-question results"
    )
    args = ap.parse_args(argv)

    if args.env_file is not None:
        load_env_file(args.env_file)

    anam = Anamnesia(base_url=args.base_url.rstrip("/"), token=args.token)
    anam.health()

    dataset = load_dataset(args.dataset)
    print(
        f"loaded {len(dataset)} {args.dataset_format} entries from {args.dataset}",
        file=sys.stderr,
    )

    args.out.parent.mkdir(parents=True, exist_ok=True)
    correct_by_type: dict[str, int] = defaultdict(int)
    total_by_type: dict[str, int] = defaultdict(int)

    if args.dataset_format == "locomo":
        questions = list(iter_questions_locomo(dataset, args.limit, args.types, args.user_prefix))
    else:
        questions = list(iter_questions(dataset, args.limit, args.types))
    # Track which Anamnesia users have already had their haystack ingested
    # this run. Lets LoCoMo's many-questions-per-conversation shape reuse
    # the same memory across all questions in one sample without re-ingesting.
    ingested_users: set[str] = set()
    total_questions = len(questions)
    run_started = time.monotonic()

    def fmt_secs(s: float) -> str:
        s = int(s)
        if s < 60:
            return f"{s}s"
        if s < 3600:
            return f"{s // 60}m{s % 60:02d}s"
        return f"{s // 3600}h{(s % 3600) // 60:02d}m"

    with args.out.open("w") as out:
        for idx, q in enumerate(questions, start=1):
            t = q.get("question_type", "unknown")
            elapsed = time.monotonic() - run_started
            eta_str = ""
            if idx > 1:
                avg = elapsed / (idx - 1)
                remaining = avg * (total_questions - idx + 1)
                eta_str = f" eta={fmt_secs(remaining)}"
            print(
                f"[{idx}/{total_questions}] start {q.get('question_id')} [{t}] "
                f"elapsed={fmt_secs(elapsed)}{eta_str}",
                file=sys.stderr,
                flush=True,
            )
            q_started = time.monotonic()
            q_user = q.get("user_id") or f"{args.user_prefix}-{q['question_id']}"
            already_ingested = q_user in ingested_users
            try:
                result = run_question(
                    q,
                    anam=anam,
                    user_prefix=args.user_prefix,
                    ingest_wait=args.ingest_wait,
                    retrieve_k=args.retrieve_k,
                    multi_query=args.multi_query,
                    multi_query_total=args.multi_query_total,
                    skip_ingest=args.skip_ingest or already_ingested,
                    ingest_mode=args.ingest_mode,
                    generate_provider=args.generate_provider,
                    generate_model=args.generate_model,
                    judge_provider=args.judge_provider,
                    judge_model=args.judge_model,
                )
                if not args.skip_ingest:
                    ingested_users.add(q_user)
            except Exception as e:
                result = {
                    "question_id": q.get("question_id"),
                    "question_type": q.get("question_type"),
                    "error": repr(e),
                    "correct": False,
                }
            out.write(json.dumps(result) + "\n")
            out.flush()
            t = result.get("question_type") or "unknown"
            total_by_type[t] += 1
            if result.get("correct"):
                correct_by_type[t] += 1
            running_correct = sum(correct_by_type.values())
            running_total = sum(total_by_type.values())
            running_acc = 100.0 * running_correct / running_total if running_total else 0.0
            q_secs = time.monotonic() - q_started
            mark = "✓" if result.get("correct") else "✗"
            err_tag = " ERROR" if result.get("error") else ""
            print(
                f"[{idx}/{total_questions}] {mark} {result.get('question_id')} [{t}] "
                f"took={fmt_secs(q_secs)} acc={running_acc:.1f}% "
                f"({running_correct}/{running_total}){err_tag}",
                file=sys.stderr,
                flush=True,
            )

    total = sum(total_by_type.values())
    correct = sum(correct_by_type.values())
    print("\n--- accuracy by question_type ---", file=sys.stderr)
    for t in sorted(total_by_type):
        c, n = correct_by_type[t], total_by_type[t]
        print(f"  {t:30s} {c}/{n}  ({100*c/n:.1f}%)", file=sys.stderr)
    if total:
        print(
            f"  {'OVERALL':30s} {correct}/{total}  ({100*correct/total:.1f}%)",
            file=sys.stderr,
        )
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
