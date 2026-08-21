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
        metadata: dict[str, Any] | None = None,
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
        if metadata:
            body["metadata"] = metadata
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

    def browse(self, domain: str, *, user: str) -> list[dict[str, Any]]:
        """Every row of one domain for a user, paged."""
        out: list[dict[str, Any]] = []
        cursor = ""
        while True:
            params: dict[str, Any] = {"user": user, "limit": 200}
            if cursor:
                params["cursor"] = cursor
            r = httpx.get(
                f"{self.base_url}/v1/{domain}",
                params=params,
                headers=self._headers(),
                timeout=self.timeout,
            )
            r.raise_for_status()
            body = r.json()
            out.extend(body.get("items") or [])
            cursor = body.get("next_cursor") or ""
            if not cursor:
                return out

    def config(self) -> list[dict[str, Any]]:
        """The server's effective settings, as /v1/config reports them."""
        r = httpx.get(
            f"{self.base_url}/v1/config", headers=self._headers(), timeout=self.timeout
        )
        r.raise_for_status()
        return r.json().get("items") or []

    def sources(self, *, user: str) -> list[dict[str, Any]]:
        """Every source row for one user, paged. Carries external_ref (the
        session id the harness ingested under), the assigned source id, and
        ops_produced, which is what separates a session the extractor
        dropped from one it stored but retrieval failed to rank."""
        out: list[dict[str, Any]] = []
        cursor = ""
        while True:
            params = {"user": user, "limit": 200}
            if cursor:
                params["cursor"] = cursor
            r = httpx.get(
                f"{self.base_url}/v1/sources",
                params=params,
                headers=self._headers(),
                timeout=self.timeout,
            )
            r.raise_for_status()
            body = r.json()
            out.extend(body.get("items") or [])
            cursor = body.get("next_cursor") or ""
            if not cursor:
                return out

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


def _chat_messages(system: str, user: str) -> list[dict[str, str]]:
    """An empty `system` means send no system turn at all. The LongMemEval
    judge prompt is a single user message, and prepending a persona to it
    grades differently."""
    msgs = []
    if system:
        msgs.append({"role": "system", "content": system})
    msgs.append({"role": "user", "content": user})
    return msgs


def call_llm(
    provider: str,
    model: str,
    system: str,
    user: str,
    *,
    max_tokens: int = 1024,
    temperature: float | None = None,
) -> str:
    estimated = max(500, (len(system) + len(user)) // 4 + 600)
    _throttle(estimated)
    if provider == "anthropic":
        import anthropic

        client = anthropic.Anthropic()
        kwargs: dict[str, Any] = {
            "model": model,
            "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": user}],
        }
        if system:
            kwargs["system"] = system
        if temperature is not None:
            kwargs["temperature"] = temperature
        msg = client.messages.create(**kwargs)
        parts = [b.text for b in msg.content if getattr(b, "type", "") == "text"]
        return "".join(parts).strip()
    if provider == "openai":
        from openai import OpenAI

        client = OpenAI()
        resp = client.chat.completions.create(
            model=model,
            messages=_chat_messages(system, user),
            max_tokens=max_tokens,
            **({} if temperature is None else {"temperature": temperature}),
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
            messages=_chat_messages(system, user),
            max_tokens=max_tokens,
            **({} if temperature is None else {"temperature": temperature}),
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

# Grading prompts copied verbatim from LongMemEval's own
# src/evaluation/evaluate_qa.py (`get_anscheck_prompt`). A single generic
# prompt is not comparable to any published LongMemEval number: it marks a
# correct refusal on an abstention question wrong, grades preference
# questions against a rubric as though it were a gold answer, and penalises
# the off-by-one day errors upstream forgives.
_JUDGE_STANDARD = "I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If the response only contains a subset of the information required by the answer, answer no. \n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only."

_JUDGE_TEMPLATES = {
    "single-session-user": _JUDGE_STANDARD,
    "single-session-assistant": _JUDGE_STANDARD,
    "multi-session": _JUDGE_STANDARD,
    "temporal-reasoning": "I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise, answer no. If the response is equivalent to the correct answer or contains all the intermediate steps to get the correct answer, you should also answer yes. If the response only contains a subset of the information required by the answer, answer no. In addition, do not penalize off-by-one errors for the number of days. If the question asks for the number of days/weeks/months, etc., and the model makes off-by-one errors (e.g., predicting 19 days when the answer is 18), the model's response is still correct. \n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only.",
    "knowledge-update": "I will give you a question, a correct answer, and a response from a model. Please answer yes if the response contains the correct answer. Otherwise, answer no. If the response contains some previous information along with an updated answer, the response should be considered as correct as long as the updated answer is the required answer.\n\nQuestion: {}\n\nCorrect Answer: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only.",
    "single-session-preference": "I will give you a question, a rubric for desired personalized response, and a response from a model. Please answer yes if the response satisfies the desired response. Otherwise, answer no. The model does not need to reflect all the points in the rubric. The response is correct as long as it recalls and utilizes the user's personal information correctly.\n\nQuestion: {}\n\nRubric: {}\n\nModel Response: {}\n\nIs the model response correct? Answer yes or no only.",
}

_JUDGE_ABSTENTION = "I will give you an unanswerable question, an explanation, and a response from a model. Please answer yes if the model correctly identifies the question as unanswerable. The model could say that the information is incomplete, or some other information is given but the asked information is not.\n\nQuestion: {}\n\nExplanation: {}\n\nModel Response: {}\n\nDoes the model correctly identify the question as unanswerable? Answer yes or no only."

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


# ---------- retrieval-only scoring ------------------------------------------
#
# LongMemEval labels every question with the haystack sessions holding its
# evidence (`answer_session_ids`). The harness ingests one session per
# /v1/ingest call with external_ref set to that session id, so /v1/sources
# gives back session id -> source id and a hit's source_id resolves to a
# session. That makes recall@k and MRR computable against gold with no
# answerer and no judge, which is both far cheaper and far less noisy than
# scoring the whole pipeline.


def index_sources(rows: list[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    """Index /v1/sources rows by the session id they were ingested under.
    Rows with no external_ref were not written by this harness and cannot
    be tied to a session, so they are dropped rather than collapsed under
    an empty key."""
    idx: dict[str, dict[str, Any]] = {}
    for r in rows:
        ext = r.get("external_ref")
        if not ext:
            continue
        idx[ext] = {
            "id": r.get("id"),
            "ops": r.get("ops_produced", 0),
            "state": r.get("extraction_state", ""),
        }
    return idx


def hit_source_ids(hits: list[dict[str, Any]]) -> list[str]:
    """The sources behind a ranked hit list, best rank first and deduped.
    Several facts extracted from one session are one retrieved source, not
    several, or recall would count the same evidence more than once. Hits
    with no source (a consolidation summary, say) cannot be scored against
    source-granularity labels and are skipped."""
    out: list[str] = []
    seen = set()
    for h in hits:
        payload = h.get("experience") or h.get("fact") or {}
        sid = payload.get("source_id")
        if not sid or sid in seen:
            continue
        seen.add(sid)
        out.append(sid)
    return out


def metric_ks(k: int) -> list[int]:
    """Cutoffs to report for a run that asked for k hits. A cutoff above k
    would label a metric computed over a truncated list with a k it never
    got. Mirrors evalMetricKs in cmd/anamnesia/eval.go."""
    ks = [v for v in (1, 5, 10, 20) if v <= k]
    if k not in ks:
        ks.append(k)
    return sorted(ks)


def score_retrieval(
    ranked: list[str], gold: set[str], ks: list[int]
) -> dict[str, Any]:
    """recall@k and MRR for one question. Refuses a question with no gold:
    scoring it 0.0 would drag the aggregate down with a labelling gap
    rather than a retrieval failure."""
    if not gold:
        raise ValueError("cannot score a question with no gold sources")
    recall = {k: len(gold & set(ranked[:k])) / len(gold) for k in ks}
    mrr = 0.0
    for i, sid in enumerate(ranked, 1):
        if sid in gold:
            mrr = 1.0 / i
            break
    return {
        "recall": recall,
        "mrr": mrr,
        "found": len(gold & set(ranked)),
        "gold": len(gold),
    }


def index_row_text(
    facts: list[dict[str, Any]], experiences: list[dict[str, Any]]
) -> tuple[dict[str, str], str]:
    """Group what was stored by the source it is attributed to, and return
    the whole corpus alongside. Rows whose provenance is missing still
    count toward the corpus: they prove the content was captured, which is
    what separates answer_elsewhere from answer_missing."""
    by_source: dict[str, list[str]] = {}
    corpus: list[str] = []
    for f in facts:
        text = f"{f.get('key', '')} {json.dumps(f.get('value'))}"
        corpus.append(text)
        if sid := f.get("source_id"):
            by_source.setdefault(sid, []).append(text)
    for e in experiences:
        text = f"{e.get('title') or ''} {e.get('body') or ''}"
        corpus.append(text)
        if sid := e.get("source_id"):
            by_source.setdefault(sid, []).append(text)
    return {k: " ".join(v) for k, v in by_source.items()}, " ".join(corpus)


_TERM_STOPWORDS = {
    "the", "a", "an", "of", "to", "in", "for", "and", "or", "is", "was",
    "were", "be", "with", "on", "at", "his", "her", "their", "they", "that",
    "this", "it", "as", "by", "from", "you", "your",
}


def answer_terms(answer: str) -> set[str]:
    """Content words of a gold answer: what has to survive extraction for
    the answer to be recoverable at all."""
    # Not every gold answer is a string: counts arrive as ints. A bare "2"
    # yields no term worth searching for, and an empty set is the right
    # answer there — classify_evidence falls back to its coarse verdict
    # rather than blaming the write path for a labelling limitation.
    words = re.findall(r"[a-z0-9]+", str(answer if answer is not None else "").lower())
    return {w for w in words if len(w) >= 3 and w not in _TERM_STOPWORDS}


def bears_answer(text: str, terms: set[str]) -> bool:
    """Whether any of the answer's content words survived into this text.

    Deliberately lenient. It backs a claim that the write path dropped
    something, and the extractor paraphrases heavily, so the bar for
    "kept" is one surviving content word. That under-reports write-path
    misses and never over-reports them: if this says the answer is gone,
    not one word of it is there."""
    low = (text or "").lower()
    return any(t in low for t in terms)


def classify_evidence(
    gold_sessions: list[str],
    index: dict[str, dict[str, Any]],
    retrieved: set[str],
    *,
    terms: set[str] | None = None,
    source_text: dict[str, str] | None = None,
    corpus_text: str = "",
) -> dict[str, str]:
    """Why each gold session did or did not reach the answerer.

    This is the attribution the end-to-end score cannot give. Pass the
    gold answer's `terms` plus the text of what was stored to separate the
    three ways a miss happens, which need opposite fixes:

      stored_not_retrieved  rows carry the answer and did not rank -> ranking
      answer_elsewhere      rows carry it, but under another source -> provenance
      answer_missing        no row anywhere carries it             -> extraction

    Without `terms` the older, coarser verdicts stand: LoCoMo has no gold
    answer to check, and ops_produced alone cannot tell these apart."""
    source_text = source_text or {}
    out: dict[str, str] = {}
    for s in gold_sessions:
        row = index.get(s)
        if row is None:
            out[s] = "not_ingested"
        elif row["id"] in retrieved:
            # An actual hit outranks a disagreeing ops_produced: something
            # demonstrably ranked, so it was stored.
            out[s] = "retrieved"
        elif not row["ops"]:
            out[s] = "not_stored"
        elif not terms:
            out[s] = "stored_not_retrieved"
        elif bears_answer(source_text.get(s, ""), terms):
            out[s] = "stored_not_retrieved"
        elif bears_answer(corpus_text, terms):
            out[s] = "answer_elsewhere"
        else:
            out[s] = "answer_missing"
    return out


EVIDENCE_STATUSES = (
    "retrieved",
    "stored_not_retrieved",
    "answer_elsewhere",
    "answer_missing",
    "not_stored",
    "not_ingested",
)


def fold_retrieval(
    acc: dict[str, Any], score: dict[str, Any], evidence: dict[str, str]
) -> None:
    """Accumulate one question's retrieval score and evidence verdicts."""
    acc["questions"] = acc.get("questions", 0) + 1
    totals = acc.setdefault("_recall_sum", {})
    for k, v in score["recall"].items():
        totals[k] = totals.get(k, 0.0) + v
    acc["_mrr_sum"] = acc.get("_mrr_sum", 0.0) + score["mrr"]
    ev = acc.setdefault("evidence", {s: 0 for s in EVIDENCE_STATUSES})
    for status in evidence.values():
        ev[status] = ev.get(status, 0) + 1
    n = acc["questions"]
    acc["recall"] = {k: v / n for k, v in totals.items()}
    acc["mrr"] = acc["_mrr_sum"] / n


def format_retrieval_summary(acc: dict[str, Any]) -> list[str]:
    """Render the retrieval score. Empty when nothing was scored."""
    n = acc.get("questions", 0)
    if not n:
        return []
    lines = ["", f"--- retrieval vs gold evidence ({n} questions) ---"]
    for k in sorted(acc["recall"]):
        lines.append(f"  {'recall@' + str(k):30s} {acc['recall'][k]:.3f}")
    lines.append(f"  {'MRR':30s} {acc['mrr']:.3f}")
    if acc.get("unscored"):
        lines.append(
            f"  {'unscored questions':30s} {acc['unscored']}  "
            f"(gold evidence absent from the store: an ingest or labelling gap, not a ranking failure)"
        )
    ev = acc.get("evidence", {})
    total = sum(ev.values())
    if total:
        lines.append("")
        lines.append(f"  gold evidence sessions: {total}")
        for s in EVIDENCE_STATUSES:
            c = ev.get(s, 0)
            lines.append(f"  {s:30s} {c}  ({100*c/total:.1f}%)")
    return lines


# ---------- graph sources ----------------------------------------------------
#
# Extractor.Run dispatches to the graph pass only for sources of kind
# "claude-session-graph"; everything else takes the fact/experience path.
# Ingesting sessions alone therefore leaves entities and edges empty no
# matter how graph.extract is set, and the channel summary then reports
# "graph 0%" for a pass that never ran. cmd/anamnesia/hook.go posts an
# extra graph source per checkpoint, and this mirrors it per session.

GRAPH_SOURCE_KIND = "claude-session-graph"


def config_flags(items: list[dict[str, Any]]) -> dict[str, str]:
    """Flatten /v1/config into key -> value."""
    return {i["key"]: i.get("value", "") for i in items if "key" in i}


def graph_enabled(flags: dict[str, str]) -> bool:
    """Whether the server runs the graph pass. Read from the server rather
    than taken as a flag, because a harness that posts graph sources the
    server ignores (or omits ones it wants) is the exact failure this
    guards: graph.extract on, zero entities, and nothing saying why."""
    return str(flags.get("graph.extract", "")).lower() == "true"


CHANNEL_KEYS = ("hits", "vector", "lexical", "graph", "graph_only", "reranked")


def hit_channels(hits: list[dict[str, Any]]) -> dict[str, int]:
    """Which retrieval channels surfaced this question's hits.

    /v1/retrieve stamps each hit with a 1-based rank per channel and omits
    the field when that channel did not reach it, so a missing key is a
    zero. `graph_only` is the number worth watching: hits neither the
    vector nor the lexical channel found, which is the only evidence that
    walking the graph earned its keep rather than re-finding what ANN and
    tsvector already had."""
    out = {k: 0 for k in CHANNEL_KEYS}
    out["hits"] = len(hits)
    for h in hits:
        vector = h.get("vector_rank", 0)
        lexical = h.get("lexical_rank", 0)
        graph = h.get("graph_rank", 0)
        if vector:
            out["vector"] += 1
        if lexical:
            out["lexical"] += 1
        if graph:
            out["graph"] += 1
            if not vector and not lexical:
                out["graph_only"] += 1
        if h.get("reranker_rank", 0):
            out["reranked"] += 1
    return out


def fold_channels(acc: dict[str, int], ch: dict[str, int]) -> None:
    """Accumulate one question's channel counts into a run total."""
    for k in CHANNEL_KEYS:
        acc[k] = acc.get(k, 0) + ch.get(k, 0)
    acc["questions"] = acc.get("questions", 0) + 1
    acc["questions_with_graph"] = acc.get("questions_with_graph", 0) + (
        1 if ch.get("graph") else 0
    )


def format_channel_summary(channels: dict[str, int]) -> list[str]:
    """Render the run's channel totals. Empty when nothing was retrieved,
    so a run where every question errored prints no table of zeroes."""
    n = channels.get("hits", 0)
    if not n:
        return []
    lines = ["", "--- retrieval channels ---", f"  {'hits shown to answerer':30s} {n}"]
    for k in ("vector", "lexical", "graph", "graph_only", "reranked"):
        v = channels.get(k, 0)
        lines.append(f"  {k:30s} {v}  ({100*v/n:.1f}% of hits)")
    qs = channels.get("questions", 0)
    g = channels.get("questions_with_graph", 0)
    if qs:
        lines.append(
            f"  {'questions with a graph hit':30s} {g}/{qs}  ({100*g/qs:.1f}%)"
        )
    return lines


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


def is_abstention(question_id: str) -> bool:
    """LongMemEval suffixes the abstention variant of a question with
    `_abs`. For those the `answer` field holds an explanation of why the
    question cannot be answered, not an answer, and the only correct model
    response is a refusal."""
    return "_abs" in question_id


def get_anscheck_prompt(
    task: str, question: str, answer: str, response: str, abstention: bool = False
) -> str:
    """Upstream raises NotImplementedError on an unrecognised task. This
    harness also runs LoCoMo, whose categories are not LongMemEval tasks,
    so an unknown task falls back to the standard recall prompt."""
    if abstention:
        return _JUDGE_ABSTENTION.format(question, answer, response)
    return _JUDGE_TEMPLATES.get(task, _JUDGE_STANDARD).format(question, answer, response)


def judge_answer(
    *,
    provider: str,
    model: str,
    question: str,
    gold: str,
    predicted: str,
    question_type: str,
    abstention: bool,
) -> dict[str, Any]:
    prompt = get_anscheck_prompt(question_type, question, gold, predicted, abstention)
    # max_tokens/temperature match upstream: the verdict is read as
    # `'yes' in response`, which only holds when the grader cannot write
    # an explanation for a 'no' that happens to contain the word 'yes'.
    raw = call_llm(provider, model, "", prompt, max_tokens=10, temperature=0)
    return {"correct": "yes" in raw.lower(), "reason": raw}


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
    mode: str = "end-to-end",
    graph_source: bool = False,
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
                resp = anam.ingest(
                    user=user,
                    content=content,
                    occurred_at=occ,
                    external_ref=ext,
                )
                if graph_source:
                    # One graph source per session, mirroring hook.go's
                    # per-checkpoint post. segment_source_ids is what ties
                    # the entity mentions to a source a search hit can
                    # carry; without it the graph is populated and
                    # unreachable.
                    sid = (resp or {}).get("source_id")
                    anam.ingest(
                        user=user,
                        content=content,
                        kind=GRAPH_SOURCE_KIND,
                        occurred_at=occ,
                        external_ref=f"{ext}-graph" if ext else None,
                        metadata={"segment_source_ids": [sid] if sid else []},
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

    if mode == "retrieval":
        # Score the ranking against the gold evidence sessions and stop.
        # No answerer, no judge: two fewer model calls per question, and
        # two fewer sources of variance between runs.
        gold_sessions = q.get("answer_session_ids") or []
        if not gold_sessions:
            raise ValueError(
                f"{q['question_id']}: no answer_session_ids, so retrieval "
                f"cannot be scored (is this a LongMemEval dataset?)"
            )
        index = index_sources(anam.sources(user=user))
        ranked = hit_source_ids(hits)
        gold_ids = {
            index[s]["id"] for s in gold_sessions if s in index and index[s]["id"]
        }
        # What was actually stored, so a miss can be attributed to ranking,
        # to provenance, or to extraction having dropped the answer.
        by_source, corpus = index_row_text(
            anam.browse("facts", user=user), anam.browse("experiences", user=user)
        )
        evidence = classify_evidence(
            gold_sessions,
            index,
            set(ranked),
            terms=answer_terms(q.get("answer", "")),
            source_text={
                s: by_source.get(index[s]["id"], "") for s in gold_sessions if s in index
            },
            corpus_text=corpus,
        )
        result = {
            "question_id": q["question_id"],
            "question_type": q.get("question_type", "unknown"),
            "abstention": is_abstention(q["question_id"]),
            "channels": hit_channels(hits),
            "question": q["question"],
            "evidence": evidence,
            "hits": hits,
        }
        if gold_ids:
            result["score"] = score_retrieval(ranked, gold_ids, metric_ks(retrieve_k))
        return result

    predicted = answer_question(
        provider=generate_provider,
        model=generate_model,
        question=q["question"],
        hits=hits,
        now_date=now_date,
    )
    abstention = is_abstention(q["question_id"])
    verdict = judge_answer(
        provider=judge_provider,
        model=judge_model,
        question=q["question"],
        gold=q["answer"],
        predicted=predicted,
        question_type=q.get("question_type", "unknown"),
        abstention=abstention,
    )
    return {
        "question_id": q["question_id"],
        "question_type": q.get("question_type", "unknown"),
        "abstention": abstention,
        "channels": hit_channels(hits),
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
        "--mode",
        choices=["end-to-end", "retrieval"],
        default="end-to-end",
        help="end-to-end = retrieve, answer, judge (the benchmark score). retrieval = score the ranking directly against each question's answer_session_ids and stop, with no answerer and no judge. Far cheaper and far less noisy, and it separates a session the extractor never stored from one retrieval failed to rank. LongMemEval datasets only.",
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
    ap.add_argument(
        "--judge-model",
        default="gpt-4o",
        help="published LongMemEval numbers are graded with gpt-4o; changing this makes a run incomparable to them, so disclose it alongside the score",
    )
    ap.add_argument(
        "--out", type=Path, required=True, help="JSONL output path for per-question results"
    )
    args = ap.parse_args(argv)

    if args.env_file is not None:
        load_env_file(args.env_file)

    anam = Anamnesia(base_url=args.base_url.rstrip("/"), token=args.token)
    anam.health()

    # Mirror the server's graph setting rather than taking it as a flag.
    # graph.extract on with no graph sources posted is a silent zero: the
    # pass never runs, and the channel summary reports "graph 0%" as
    # though the graph had been measured and found wanting.
    graph_source = graph_enabled(config_flags(anam.config()))
    print(
        f"graph.extract is {'on' if graph_source else 'off'} on the server: "
        f"{'posting' if graph_source else 'not posting'} a {GRAPH_SOURCE_KIND} "
        f"source per session",
        file=sys.stderr,
    )
    if graph_source and args.ingest_mode == "raw":
        print(
            "  note: --ingest-mode raw bypasses the extractor entirely, so no "
            "graph pass runs regardless",
            file=sys.stderr,
        )

    dataset = load_dataset(args.dataset)
    print(
        f"loaded {len(dataset)} {args.dataset_format} entries from {args.dataset}",
        file=sys.stderr,
    )

    args.out.parent.mkdir(parents=True, exist_ok=True)
    correct_by_type: dict[str, int] = defaultdict(int)
    total_by_type: dict[str, int] = defaultdict(int)
    # Abstention questions keep their parent question_type in the buckets
    # above, matching upstream. These two track them as their own ability,
    # which is the only way to see whether the answerer refuses or invents.
    correct_abs = 0
    total_abs = 0
    # Which retrieval channels actually fed the answerer, summed over
    # every question that got as far as retrieving.
    channels: dict[str, int] = {}
    # --mode retrieval: recall/MRR against gold evidence, and why each gold
    # session did or did not reach the answerer.
    retrieval: dict[str, Any] = {}

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
                    mode=args.mode,
                    graph_source=graph_source,
                )
                if not args.skip_ingest:
                    ingested_users.add(q_user)
            except Exception as e:
                result = {
                    "question_id": q.get("question_id"),
                    "question_type": q.get("question_type"),
                    "abstention": is_abstention(q.get("question_id") or ""),
                    "error": repr(e),
                    "correct": False,
                }
            out.write(json.dumps(result) + "\n")
            out.flush()
            t = result.get("question_type") or "unknown"
            if result.get("channels"):
                fold_channels(channels, result["channels"])
            q_secs = time.monotonic() - q_started
            err_tag = " ERROR" if result.get("error") else ""

            if args.mode == "retrieval":
                # No judge ran, so there is no ✓/✗ to show: report the
                # question's own recall and the running mean instead.
                if result.get("score"):
                    fold_retrieval(
                        retrieval, result["score"], result.get("evidence") or {}
                    )
                elif not result.get("error"):
                    retrieval["unscored"] = retrieval.get("unscored", 0) + 1
                top = max(retrieval.get("recall") or {0: 0}) or 0
                here = (result.get("score") or {}).get("recall", {}).get(top)
                running = (retrieval.get("recall") or {}).get(top)
                print(
                    f"[{idx}/{total_questions}] {result.get('question_id')} [{t}] "
                    f"took={fmt_secs(q_secs)} "
                    f"r@{top}={'--' if here is None else f'{here:.2f}'} "
                    f"mean={'--' if running is None else f'{running:.3f}'}{err_tag}",
                    file=sys.stderr,
                    flush=True,
                )
            else:
                total_by_type[t] += 1
                if result.get("correct"):
                    correct_by_type[t] += 1
                if result.get("abstention"):
                    total_abs += 1
                    if result.get("correct"):
                        correct_abs += 1
                running_correct = sum(correct_by_type.values())
                running_total = sum(total_by_type.values())
                running_acc = (
                    100.0 * running_correct / running_total if running_total else 0.0
                )
                mark = "✓" if result.get("correct") else "✗"
                print(
                    f"[{idx}/{total_questions}] {mark} {result.get('question_id')} [{t}] "
                    f"took={fmt_secs(q_secs)} acc={running_acc:.1f}% "
                    f"({running_correct}/{running_total}){err_tag}",
                    file=sys.stderr,
                    flush=True,
                )

    for line in format_retrieval_summary(retrieval):
        print(line, file=sys.stderr)

    total = sum(total_by_type.values())
    correct = sum(correct_by_type.values())
    if total:
        print("\n--- accuracy by question_type ---", file=sys.stderr)
    for t in sorted(total_by_type):
        c, n = correct_by_type[t], total_by_type[t]
        print(f"  {t:30s} {c}/{n}  ({100*c/n:.1f}%)", file=sys.stderr)
    if total_abs:
        print(
            f"  {'(abstention, also above)':30s} {correct_abs}/{total_abs}  "
            f"({100*correct_abs/total_abs:.1f}%)",
            file=sys.stderr,
        )
    if total:
        print(
            f"  {'OVERALL':30s} {correct}/{total}  ({100*correct/total:.1f}%)",
            file=sys.stderr,
        )

    for line in format_channel_summary(channels):
        print(line, file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
