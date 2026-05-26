"""LongMemEval harness against a running Anamnesia stack.

Pipeline per question:
  1. mint a fresh user (`lme-<question_id>`) so memory does not leak across questions
  2. POST each haystack session to /v1/ingest with the matching haystack_date as occurred_at
  3. wait `--ingest-wait` seconds for the extractor worker to drain the queue
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
    """LongMemEval ships dates like '2023/05/20 (Sat) 02:21'. The Anamnesia
    server expects RFC3339, so strip the weekday and emit UTC."""
    if not stamp:
        return None
    cleaned = _DOW.sub(" ", stamp).strip()
    for fmt in ("%Y/%m/%d %H:%M", "%Y/%m/%d %H:%M:%S"):
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

    def retrieve(self, *, user: str, prompt: str, k: int = 0) -> list[dict[str, Any]]:
        body: dict[str, Any] = {"user": user, "prompt": prompt}
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


# ---------- LLM wrappers ----------------------------------------------------


def call_llm(provider: str, model: str, system: str, user: str) -> str:
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


def render_hits(hits: list[dict[str, Any]]) -> str:
    if not hits:
        return "(no memory snippets retrieved)"
    lines = []
    for i, h in enumerate(hits, 1):
        body = h.get("body") or h.get("text") or json.dumps(h)
        lines.append(f"[{i}] {body}")
    return "\n".join(lines)


def answer_question(
    *, provider: str, model: str, question: str, hits: list[dict[str, Any]]
) -> str:
    user = f"Memory snippets:\n{render_hits(hits)}\n\nQuestion: {question}"
    return call_llm(provider, model, ANSWER_SYSTEM, user)


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
    generate_provider: str,
    generate_model: str,
    judge_provider: str,
    judge_model: str,
) -> dict[str, Any]:
    user = f"{user_prefix}-{q['question_id']}"
    sessions: list[list[dict[str, Any]]] = q.get("haystack_sessions", [])
    dates: list[str] = q.get("haystack_dates", [])
    ids: list[str] = q.get("haystack_session_ids", [])

    if not skip_ingest:
        for i, session in enumerate(sessions):
            content = session_to_text(session)
            if not content.strip():
                continue
            anam.ingest(
                user=user,
                content=content,
                occurred_at=to_rfc3339(dates[i] if i < len(dates) else None),
                external_ref=ids[i] if i < len(ids) else None,
            )

        if ingest_wait > 0:
            time.sleep(ingest_wait)

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
    predicted = answer_question(
        provider=generate_provider,
        model=generate_model,
        question=q["question"],
        hits=hits,
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
        default=15.0,
        help="seconds to sleep after ingesting all sessions, so the extractor worker drains",
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
    ap.add_argument("--limit", type=int, default=None)
    ap.add_argument(
        "--types",
        nargs="*",
        default=None,
        help="restrict to question_type values (e.g. temporal-reasoning knowledge-update)",
    )
    ap.add_argument(
        "--generate-provider", choices=["anthropic", "openai"], default="anthropic"
    )
    ap.add_argument("--generate-model", default="claude-sonnet-4-6")
    ap.add_argument(
        "--judge-provider", choices=["anthropic", "openai"], default="anthropic"
    )
    ap.add_argument("--judge-model", default="claude-sonnet-4-6")
    ap.add_argument(
        "--out", type=Path, required=True, help="JSONL output path for per-question results"
    )
    args = ap.parse_args(argv)

    if args.env_file is not None:
        load_env_file(args.env_file)

    anam = Anamnesia(base_url=args.base_url.rstrip("/"), token=args.token)
    anam.health()

    dataset = load_dataset(args.dataset)
    print(f"loaded {len(dataset)} questions from {args.dataset}", file=sys.stderr)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    correct_by_type: dict[str, int] = defaultdict(int)
    total_by_type: dict[str, int] = defaultdict(int)

    with args.out.open("w") as out:
        for q in iter_questions(dataset, args.limit, args.types):
            try:
                result = run_question(
                    q,
                    anam=anam,
                    user_prefix=args.user_prefix,
                    ingest_wait=args.ingest_wait,
                    retrieve_k=args.retrieve_k,
                    multi_query=args.multi_query,
                    multi_query_total=args.multi_query_total,
                    skip_ingest=args.skip_ingest,
                    generate_provider=args.generate_provider,
                    generate_model=args.generate_model,
                    judge_provider=args.judge_provider,
                    judge_model=args.judge_model,
                )
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
            mark = "✓" if result.get("correct") else "✗"
            print(
                f"{mark} {result.get('question_id')} [{t}]",
                file=sys.stderr,
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
