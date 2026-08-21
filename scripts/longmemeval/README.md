# LongMemEval harness

Runs the [LongMemEval](https://github.com/xiaowu0162/longmemeval) benchmark
against a running Anamnesia stack, and
[LoCoMo](https://github.com/snap-research/locomo) via `--dataset-format
locomo`. Per question it ingests all haystack sessions through
`/v1/ingest`, waits for the extractor to drain, queries `/v1/retrieve`,
asks an LLM to answer using only those hits, and asks an LLM judge to
score the answer against the gold one.

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
   # a faster extractor poll drains the queue sooner, so each question
   # spends less time waiting before it retrieves:
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
  --generate-provider anthropic --generate-model claude-sonnet-4-6 \
  --judge-provider openai       --judge-model    gpt-4o \
  --out ./out/lme-run.jsonl
```

Per-question JSON lines go to `--out`; accuracy and retrieval-channel
breakdowns print to stderr at the end (see [Reading the
output](#reading-the-output)).

## Tests

The judge half is covered by parity tests against the prompts in
LongMemEval's own evaluation script:

```bash
docker run --rm -v "$PWD/scripts/longmemeval:/w" -w /w python:3.13-slim \
  sh -c "pip install -q pytest 'httpx>=0.27' && python -m pytest -q"
```

## Flags

### What to run

- `--dataset PATH` (required) and `--out PATH` (required): the input JSON
  and the JSONL trace.
- `--dataset-format longmemeval|locomo` (default `longmemeval`).
  `longmemeval` is one question per top-level entry, each with its own
  haystack. `locomo` reads `snap-research/locomo10.json`, where each
  sample is one multi-session conversation carrying many QA pairs: the
  conversation is ingested once per sample and every QA is asked against
  that shared memory.
- `--limit N`: stop after N questions. No limit by default.
- `--types temporal-reasoning knowledge-update`: restrict to a subset of
  `question_type` values to debug one ability.

### Connection

- `--base-url` (default `http://localhost:8181`, or `ANAMNESIA_BASE_URL`).
- `--token` (default `ANAMNESIA_SERVER_TOKEN`).
- `--env-file PATH`: read a Docker-style `.env` for `OPENAI_API_KEY`,
  `ANTHROPIC_API_KEY` and `ANAMNESIA_SERVER_TOKEN` before the client is
  built. Real environment variables still win.

### Ingest

- `--user-prefix lme` (default `lme`): each question runs as user
  `lme-<question_id>` so memory does not leak across questions.
- `--ingest-mode extract|raw` (default `extract`). `extract` is
  Anamnesia's real pipeline: POST `/v1/ingest`, wait for the LLM
  extractor, retrieve the facts and experiences it produced. `raw` POSTs
  each session to `/v1/experience` verbatim and embeds inline, which is
  a pure RAG baseline with no extractor and no queue to wait for. Run
  both: the gap between them is what extraction is worth.
- `--ingest-wait` (default 600): upper bound, in seconds, on the wait for
  the extract and embed queues to drain after ingesting a question's
  sessions. The harness polls `/v1/queue/pending` every 2s and retrieves
  the moment both hit 0, so this only caps the worst case and raising it
  costs nothing on a healthy stack. If the bound is hit the harness
  prints `queue did not drain within Ns` and scores that question against
  a partially warm index.
- `--skip-ingest`: do not POST at all, reusing whatever already exists
  for each per-question user. Makes re-runs of retrieve/answer/judge
  cheap after one ingestion pass.

### Mode

- `--mode end-to-end|retrieval` (default `end-to-end`).

  `end-to-end` is the benchmark score: retrieve, answer, judge.

  `retrieval` scores the ranking directly against each question's
  `answer_session_ids` and stops. No answerer, no judge, so it costs
  nothing per question beyond the retrieve call and carries none of the
  two models' variance. LongMemEval datasets only, since LoCoMo has no
  evidence labels; the harness refuses rather than reporting recall 0 for
  every question.

  It works because the harness ingests one session per `/v1/ingest` call
  with `external_ref` set to the session id, so `/v1/sources` gives back
  session id to source id and a hit's `source_id` resolves to a session.

  Pair it with `--skip-ingest`: ingest a subset once, then re-score any
  retrieval-ranking change for free, as often as you like. Only a change
  to *extraction* invalidates the stored corpus and needs a re-ingest.

### Retrieve

- `--retrieve-k` (default 20): hits fetched per query. The server's own
  default is 10.
- `--multi-query` / `--no-multi-query` (default off): expand each
  question into LLM-generated sub-queries and union the hits. Off because
  it empirically adds noise that hurts the answerer more often than it
  helps at small sample sizes. Useful for counting and aggregation
  questions, harmful for direct-fact ones.
- `--multi-query-total` (default 40): cap on unique hits handed to the
  answerer when `--multi-query` is on.

### Answer and judge

- `--generate-provider anthropic|openai|openrouter` (default `anthropic`)
  and `--generate-model` (default `claude-sonnet-4-6`).
- `--judge-provider` (default `openai`) and `--judge-model` (default
  `gpt-4o`). See the caveats below before changing the judge.

## Reading the output

Two summaries print to stderr at the end. The first is accuracy per
`question_type`, plus the abstention line and an overall.

The second attributes the hits the answerer actually saw to the retrieval
channel that surfaced them:

```
--- retrieval channels ---
  hits shown to answerer         5
  vector                         3  (60.0% of hits)
  lexical                        2  (40.0% of hits)
  graph                          1  (20.0% of hits)
  graph_only                     1  (20.0% of hits)
  reranked                       5  (100.0% of hits)
  questions with a graph hit     1/2  (50.0%)
```

`graph_only` is the line to watch. `/v1/retrieve` stamps every hit with a
1-based rank per channel, so a hit with a `graph_rank` but no
`vector_rank` or `lexical_rank` is one the graph walk reached and neither
ANN nor tsvector did. That count is the evidence that walking the graph
adds recall rather than re-finding what the other two channels already
had. A run where `graph` is high but `graph_only` is near zero means the
graph is confirming, not contributing.

The same counts land per question under `channels` in the `--out` JSONL,
so a low `graph_only` can be traced back to the questions it came from.

Under `--mode retrieval` a third summary replaces the accuracy one:

```
--- retrieval vs gold evidence (3 questions) ---
  recall@1                       0.500
  recall@5                       0.500
  MRR                            0.667

  gold evidence sessions: 4
  retrieved                      2  (50.0%)
  stored_not_retrieved           1  (25.0%)
  not_stored                     1  (25.0%)
  not_ingested                   0  (0.0%)
```

The bottom block is the attribution an accuracy score cannot give. Each
status names a different failure, and they call for opposite fixes:

| status | meaning | where to look |
|---|---|---|
| `retrieved` | the gold session's rows ranked | nothing to fix |
| `stored_not_retrieved` | rows carry the answer but missed the cutoff | ranking |
| `answer_elsewhere` | rows carry it, but attributed to another source | provenance |
| `answer_missing` | no row anywhere carries the answer | extraction |
| `not_stored` | the session produced no rows at all | the surprise gate |
| `not_ingested` | the session never reached the store | ingest |

The three middle rows need the gold answer text, so they only appear for
LongMemEval. `bears_answer` decides them by asking whether **any** content
word of the answer survived into the stored rows. That is deliberately
lenient, because the extractor paraphrases heavily: it under-reports
write-path misses and never over-reports them. When it says the answer is
missing, not one word of it is there.

`ops_produced > 0` alone cannot make this call, which is why it used to be
wrong: a session can extract eight operations and keep none of the thing
it was asked about. That is what LongMemEval question `58bf7951` does, and
it was being reported as a ranking failure.

`unscored questions`, when it appears, is none of the above: the gold
sessions are absent from the store entirely, an ingest or labelling gap.

Per-question `score` and `evidence` land in the JSONL alongside them.

## Caveats

- The answerer sees exactly what `/v1/retrieve` ranked: top 20 by
  default (`--retrieve-k`), with no re-ranking or filtering on top.
  `--multi-query` is the one exception, and it is off by default.
- The judge mirrors LongMemEval's own `src/evaluation/evaluate_qa.py`:
  one grading prompt per `question_type`, a different one for the 30
  abstention questions (`_abs` in the id, where the gold `answer` is an
  explanation and the only correct response is a refusal), and a bare
  yes/no read back under `max_tokens=10`. `--judge-model` defaults to
  `gpt-4o` because that is what published numbers are graded with;
  anything else is a run you cannot compare to them, so disclose it.
- Abstention questions keep their parent `question_type` in the
  breakdown, matching upstream. They are also totalled on their own
  `(abstention, also above)` line, which is the one number that says
  whether the answerer refuses or invents.
