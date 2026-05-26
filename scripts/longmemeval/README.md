# LongMemEval harness

Runs the [LongMemEval](https://github.com/xiaowu0162/longmemeval) benchmark
against a running Anamnesia stack. Per question it ingests all haystack
sessions through `/v1/ingest`, waits for the extractor to drain, queries
`/v1/retrieve`, asks an LLM to answer using only those hits, and asks an
LLM judge to score the answer against the gold one.

## Prerequisites

1. **A running Anamnesia stack.**
   ```bash
   make up && ./bin/anamnesia doctor
   ```
   Make sure the extractor is configured for real LLM work (`stub` will NOOP
   everything):
   ```env
   ANAMNESIA_LLM_PROVIDER=anthropic
   ANTHROPIC_API_KEY=sk-ant-…
   ANAMNESIA_EMBED_PROVIDER=openai
   OPENAI_API_KEY=sk-…
   # speed up the extractor so --ingest-wait stays small:
   ANAMNESIA_EXTRACT_EVERY=1s
   ```

2. **The dataset.** Download one of the JSON splits from
   <https://huggingface.co/datasets/xiaowu0162/longmemeval>:
   ```bash
   # smallest variant (~115k tokens of history per question, 500 questions)
   huggingface-cli download xiaowu0162/longmemeval \
     longmemeval_s.json --local-dir ./data
   ```

3. **Python deps.**
   ```bash
   pip install -r scripts/longmemeval/requirements.txt
   ```

## Run

```bash
python scripts/longmemeval/harness.py \
  --dataset ./data/longmemeval_s.json \
  --limit 20 \
  --ingest-wait 30 \
  --generate-provider anthropic --generate-model claude-sonnet-4-6 \
  --judge-provider anthropic    --judge-model    claude-sonnet-4-6 \
  --out ./out/lme-run.jsonl
```

Per-question JSON lines go to `--out`; a per-`question_type` accuracy
breakdown is printed to stderr at the end.

## Flags worth knowing

- `--ingest-wait` — seconds to sleep after ingesting all sessions for a
  question, before retrieving. Must be ≥ the extractor's poll interval
  plus the time it takes to drain the queue. Start at 30s; tune down.
- `--types temporal-reasoning knowledge-update` — restrict to subsets
  to debug specific abilities.
- `--user-prefix lme` — each question runs as user
  `lme-<question_id>` so memory does not leak across questions.
- `ANAMNESIA_BASE_URL` / `ANAMNESIA_SERVER_TOKEN` env vars are
  honoured if the corresponding flags are omitted.

## Caveats

- Async extraction means `--ingest-wait` is a sleep, not a barrier.
  If you see degraded scores, raise it before tuning anything else.
- `/v1/retrieve` returns top-10 hits with default scoring. The harness
  does not currently re-rank or filter — what comes out of Anamnesia
  is what the answerer sees.
- The judge is LLM-as-judge against `answer`. Published LongMemEval
  numbers use the same approach with GPT-4o; swap `--judge-model` if
  you want strict parity.
