"""Parity tests for the judge half of the harness.

The reference is LongMemEval's own `src/evaluation/evaluate_qa.py`, which
picks a grading prompt per `(question_type, abstention)` and reads a bare
yes/no back. Anything we score with a single generic prompt is not
comparable to a published LongMemEval number, so these tests pin the
selection rules rather than the wording.
"""

from __future__ import annotations

import harness


# ---------- abstention detection -------------------------------------------


def test_abstention_flag_comes_from_question_id_suffix():
    assert harness.is_abstention("gpt4_038cae2a_abs") is True


def test_plain_question_id_is_not_abstention():
    assert harness.is_abstention("gpt4_038cae2a") is False


# ---------- prompt selection ------------------------------------------------


def test_abstention_prompt_asks_whether_the_question_is_unanswerable():
    prompt = harness.get_anscheck_prompt(
        "multi-session", "q", "a", "r", abstention=True
    )
    assert "unanswerable" in prompt


def test_abstention_prompt_wins_over_every_question_type():
    for task in [
        "single-session-user",
        "single-session-assistant",
        "multi-session",
        "temporal-reasoning",
        "knowledge-update",
        "single-session-preference",
    ]:
        prompt = harness.get_anscheck_prompt(task, "q", "a", "r", abstention=True)
        assert "unanswerable" in prompt, task


def test_temporal_reasoning_prompt_forgives_off_by_one_days():
    prompt = harness.get_anscheck_prompt("temporal-reasoning", "q", "a", "r")
    assert "off-by-one" in prompt


def test_knowledge_update_prompt_accepts_a_response_carrying_the_stale_value():
    prompt = harness.get_anscheck_prompt("knowledge-update", "q", "a", "r")
    assert "previous information along with an updated answer" in prompt


def test_preference_prompt_grades_against_a_rubric_not_a_gold_answer():
    prompt = harness.get_anscheck_prompt("single-session-preference", "q", "a", "r")
    assert "Rubric:" in prompt
    assert "Correct Answer:" not in prompt


def test_plain_recall_types_get_the_standard_prompt():
    for task in ["single-session-user", "single-session-assistant", "multi-session"]:
        prompt = harness.get_anscheck_prompt(task, "q", "a", "r")
        assert "Correct Answer:" in prompt, task
        assert "off-by-one" not in prompt, task
        assert "Rubric:" not in prompt, task


def test_locomo_question_types_fall_back_to_the_standard_prompt():
    """The harness also runs LoCoMo, whose categories are not LongMemEval
    tasks. Upstream raises here; we grade them like plain recall."""
    prompt = harness.get_anscheck_prompt("multi-hop", "q", "a", "r")
    assert "Correct Answer:" in prompt


def test_prompt_interpolates_question_gold_and_response():
    prompt = harness.get_anscheck_prompt(
        "multi-session", "QQQ", "GGG", "RRR", abstention=False
    )
    assert "QQQ" in prompt and "GGG" in prompt and "RRR" in prompt


def test_abstention_prompt_interpolates_question_explanation_and_response():
    prompt = harness.get_anscheck_prompt(
        "multi-session", "QQQ", "GGG", "RRR", abstention=True
    )
    assert "QQQ" in prompt and "GGG" in prompt and "RRR" in prompt


# ---------- verdict parsing -------------------------------------------------


class _RecordingLLM:
    """Stands in for call_llm and records how the judge asked for its grade."""

    def __init__(self, reply: str):
        self.reply = reply
        self.calls: list[dict] = []

    def __call__(self, provider, model, system, user, **kwargs):
        self.calls.append(
            {"provider": provider, "model": model, "system": system, "user": user, **kwargs}
        )
        return self.reply


def _judge(monkeypatch, reply, **overrides):
    stub = _RecordingLLM(reply)
    monkeypatch.setattr(harness, "call_llm", stub)
    kwargs = dict(
        provider="openai",
        model="gpt-4o",
        question="q",
        gold="a",
        predicted="r",
        question_type="multi-session",
        abstention=False,
    )
    kwargs.update(overrides)
    return stub, harness.judge_answer(**kwargs)


def test_judge_scores_a_yes_as_correct(monkeypatch):
    _, verdict = _judge(monkeypatch, "yes")
    assert verdict["correct"] is True


def test_judge_scores_a_no_as_incorrect(monkeypatch):
    _, verdict = _judge(monkeypatch, "no")
    assert verdict["correct"] is False


def test_judge_tolerates_capitalisation_and_punctuation(monkeypatch):
    _, verdict = _judge(monkeypatch, "Yes.")
    assert verdict["correct"] is True


def test_judge_keeps_the_raw_grade_as_the_reason(monkeypatch):
    _, verdict = _judge(monkeypatch, "no")
    assert verdict["reason"] == "no"


def test_judge_constrains_the_grade_to_a_short_deterministic_completion(monkeypatch):
    """Upstream reads `'yes' in response` under max_tokens=10. Without the
    same cap a chatty grader can smuggle a 'yes' into an explanation and
    every wrong answer scores."""
    stub, _ = _judge(monkeypatch, "yes")
    call = stub.calls[0]
    assert call["max_tokens"] <= 10
    assert call["temperature"] == 0


def test_judge_sends_the_grading_prompt_as_the_user_turn(monkeypatch):
    stub, _ = _judge(monkeypatch, "yes")
    call = stub.calls[0]
    assert "Correct Answer:" in call["user"]
    assert not call["system"]


def test_judge_uses_the_abstention_prompt_when_flagged(monkeypatch):
    stub, _ = _judge(monkeypatch, "yes", abstention=True)
    assert "unanswerable" in stub.calls[0]["user"]


def test_judge_uses_the_question_type_prompt(monkeypatch):
    stub, _ = _judge(monkeypatch, "yes", question_type="temporal-reasoning")
    assert "off-by-one" in stub.calls[0]["user"]


# ---------- retrieval channel attribution -----------------------------------
#
# /v1/retrieve stamps each hit with a 1-based rank per channel and omits
# the field when that channel did not surface it. Retrieval's own comment
# on the fuse step is the contract these lean on: "a graph-sourced hit
# (vector_rank 0, lexical_rank 0) is identifiable as 'the graph found
# this'".


def test_channels_count_a_vector_only_hit():
    ch = harness.hit_channels([{"vector_rank": 1}])
    assert ch["hits"] == 1
    assert ch["vector"] == 1
    assert ch["lexical"] == 0
    assert ch["graph"] == 0


def test_channels_credit_every_channel_that_surfaced_one_hit():
    ch = harness.hit_channels([{"vector_rank": 3, "lexical_rank": 1, "graph_rank": 2}])
    assert ch["vector"] == 1 and ch["lexical"] == 1 and ch["graph"] == 1


def test_a_missing_rank_key_means_that_channel_did_not_surface_it():
    """The Go fields are omitempty, so an absent key is a zero rank."""
    ch = harness.hit_channels([{"vector_rank": 1}])
    assert ch["graph"] == 0 and ch["reranked"] == 0


def test_graph_only_counts_a_hit_no_other_channel_reached():
    ch = harness.hit_channels([{"graph_rank": 1}])
    assert ch["graph"] == 1
    assert ch["graph_only"] == 1


def test_graph_only_excludes_a_hit_vector_also_reached():
    ch = harness.hit_channels([{"graph_rank": 1, "vector_rank": 4}])
    assert ch["graph"] == 1
    assert ch["graph_only"] == 0


def test_graph_only_excludes_a_hit_lexical_also_reached():
    ch = harness.hit_channels([{"graph_rank": 1, "lexical_rank": 4}])
    assert ch["graph_only"] == 0


def test_channels_count_reranked_hits():
    ch = harness.hit_channels([{"vector_rank": 1, "reranker_rank": 2}, {"vector_rank": 2}])
    assert ch["reranked"] == 1


def test_channels_of_no_hits_are_all_zero():
    ch = harness.hit_channels([])
    assert set(ch.values()) == {0}


def test_fold_sums_each_channel_across_questions():
    acc = {}
    harness.fold_channels(acc, harness.hit_channels([{"vector_rank": 1}]))
    harness.fold_channels(acc, harness.hit_channels([{"vector_rank": 1}, {"lexical_rank": 1}]))
    assert acc["hits"] == 3
    assert acc["vector"] == 2
    assert acc["lexical"] == 1


def test_fold_counts_the_questions_it_folded():
    acc = {}
    harness.fold_channels(acc, harness.hit_channels([]))
    harness.fold_channels(acc, harness.hit_channels([{"vector_rank": 1}]))
    assert acc["questions"] == 2


def test_fold_counts_questions_where_the_graph_contributed_at_all():
    acc = {}
    harness.fold_channels(acc, harness.hit_channels([{"vector_rank": 1}]))
    harness.fold_channels(acc, harness.hit_channels([{"vector_rank": 1}, {"graph_rank": 1}]))
    harness.fold_channels(acc, harness.hit_channels([{"graph_rank": 2}]))
    assert acc["questions"] == 3
    assert acc["questions_with_graph"] == 2


def _summary(hit_lists):
    acc = {}
    for hits in hit_lists:
        harness.fold_channels(acc, harness.hit_channels(hits))
    return "\n".join(harness.format_channel_summary(acc))


def test_summary_reports_every_channel():
    text = _summary([[{"vector_rank": 1, "lexical_rank": 1, "graph_rank": 1}]])
    for k in ("vector", "lexical", "graph", "graph_only", "reranked"):
        assert k in text


def test_summary_reports_the_graph_only_count():
    text = _summary([[{"vector_rank": 1}, {"graph_rank": 1}]])
    graph_only = [ln for ln in text.splitlines() if "graph_only" in ln][0]
    assert "1" in graph_only


def test_summary_reports_how_many_questions_the_graph_reached():
    text = _summary([[{"vector_rank": 1}], [{"graph_rank": 1}]])
    line = [ln for ln in text.splitlines() if "questions with a graph hit" in ln][0]
    assert "1/2" in line


def test_summary_is_empty_when_no_question_retrieved_anything():
    """A run where every question errored must not print a channel table
    of zeroes, and must not divide by them either."""
    assert harness.format_channel_summary({}) == []
    assert harness.format_channel_summary(
        {"hits": 0, "questions": 3, "questions_with_graph": 0}
    ) == []


# ---------- retrieval-only scoring -------------------------------------------
#
# LongMemEval labels each question with the haystack sessions holding its
# evidence (`answer_session_ids`). The harness ingests one session per
# /v1/ingest call with external_ref set to the session id, so a hit's
# source_id resolves back to a session and retrieval can be scored against
# gold with no answerer and no judge in the loop.


def _src(ext, sid, ops=1, state="done"):
    return {"external_ref": ext, "id": sid, "ops_produced": ops, "extraction_state": state}


def test_source_index_keys_on_external_ref():
    idx = harness.index_sources([_src("sess-a", "uuid-1")])
    assert idx["sess-a"]["id"] == "uuid-1"


def test_source_index_skips_rows_with_no_external_ref():
    """Sources the harness did not create (or created without a ref) cannot
    be tied to a session and must not collide under an empty key."""
    idx = harness.index_sources([_src("", "uuid-1"), {"id": "uuid-2"}])
    assert idx == {}


def test_source_index_carries_ops_and_state():
    idx = harness.index_sources([_src("sess-a", "uuid-1", ops=0, state="skipped")])
    assert idx["sess-a"]["ops"] == 0
    assert idx["sess-a"]["state"] == "skipped"


def test_hit_source_ids_reads_both_domains_in_rank_order():
    ranked = harness.hit_source_ids(
        [{"experience": {"source_id": "s1"}}, {"fact": {"source_id": "s2"}}]
    )
    assert ranked == ["s1", "s2"]


def test_hit_source_ids_dedupes_keeping_the_best_rank():
    """Several facts from one session are one retrieved source, not three,
    or recall would count the same evidence repeatedly."""
    ranked = harness.hit_source_ids(
        [{"fact": {"source_id": "s1"}}, {"fact": {"source_id": "s1"}}, {"fact": {"source_id": "s2"}}]
    )
    assert ranked == ["s1", "s2"]


def test_hit_source_ids_skips_hits_with_no_source():
    """A consolidation summary has no source and cannot be scored against
    source-granularity labels."""
    ranked = harness.hit_source_ids([{"experience": {}}, {"fact": {"source_id": "s1"}}])
    assert ranked == ["s1"]


def test_metric_ks_never_exceed_the_requested_k():
    assert harness.metric_ks(5) == [1, 5]
    assert harness.metric_ks(20) == [1, 5, 10, 20]


def test_metric_ks_fall_back_to_k_when_every_cutoff_is_too_large():
    assert harness.metric_ks(3) == [1, 3]
    assert harness.metric_ks(1) == [1]


def test_recall_counts_gold_sources_inside_the_cutoff():
    s = harness.score_retrieval(["a", "b", "c"], {"a", "c"}, [1, 5])
    assert s["recall"][1] == 0.5
    assert s["recall"][5] == 1.0


def test_recall_is_zero_when_nothing_gold_was_retrieved():
    s = harness.score_retrieval(["x", "y"], {"a"}, [5])
    assert s["recall"][5] == 0.0
    assert s["mrr"] == 0.0


def test_mrr_is_the_reciprocal_rank_of_the_first_gold_hit():
    assert harness.score_retrieval(["x", "a"], {"a"}, [5])["mrr"] == 0.5
    assert harness.score_retrieval(["a", "x"], {"a"}, [5])["mrr"] == 1.0


def test_score_reports_how_much_gold_it_found():
    s = harness.score_retrieval(["a"], {"a", "b"}, [5])
    assert s["found"] == 1 and s["gold"] == 2


def test_scoring_a_question_with_no_gold_is_refused():
    """Silently scoring it 0.0 would drag an aggregate down with a labelling
    gap rather than a retrieval failure."""
    try:
        harness.score_retrieval(["a"], set(), [5])
    except ValueError:
        return
    raise AssertionError("expected ValueError")


def test_evidence_that_ranked_is_a_hit():
    idx = harness.index_sources([_src("sess-a", "u1")])
    assert harness.classify_evidence(["sess-a"], idx, {"u1"}) == {"sess-a": "retrieved"}


def test_evidence_stored_but_unranked_is_a_retrieval_miss():
    idx = harness.index_sources([_src("sess-a", "u1", ops=3)])
    assert harness.classify_evidence(["sess-a"], idx, set()) == {
        "sess-a": "stored_not_retrieved"
    }


def test_evidence_that_produced_no_rows_is_a_write_path_miss():
    """The surprise gate dropped it or extraction yielded nothing. Retrieval
    never had a chance, so blaming ranking for this would be wrong."""
    idx = harness.index_sources([_src("sess-a", "u1", ops=0, state="skipped")])
    assert harness.classify_evidence(["sess-a"], idx, set()) == {"sess-a": "not_stored"}


def test_evidence_missing_from_the_store_entirely_is_its_own_status():
    assert harness.classify_evidence(["sess-a"], {}, set()) == {"sess-a": "not_ingested"}


def test_a_retrieved_source_counts_as_retrieved_even_with_zero_ops():
    """ops_produced disagreeing with an actual hit is a server-side
    inconsistency; trust the hit, since something demonstrably ranked."""
    idx = harness.index_sources([_src("sess-a", "u1", ops=0)])
    assert harness.classify_evidence(["sess-a"], idx, {"u1"}) == {"sess-a": "retrieved"}


def _fold(*questions):
    acc = {}
    for ranked, gold_sessions, idx in questions:
        gold_ids = {idx[s]["id"] for s in gold_sessions if s in idx}
        score = harness.score_retrieval(ranked, gold_ids or {"__none__"}, [1, 5])
        harness.fold_retrieval(acc, score, harness.classify_evidence(gold_sessions, idx, set(ranked)))
    return acc


def test_fold_averages_recall_across_questions():
    idx = harness.index_sources([_src("s", "u1")])
    acc = _fold((["u1"], ["s"], idx), ([], ["s"], idx))
    assert acc["questions"] == 2
    assert acc["recall"][5] == 0.5


def test_fold_tallies_evidence_status_across_questions():
    idx = harness.index_sources([_src("s", "u1", ops=0)])
    acc = _fold(([], ["s"], idx), ([], ["s"], idx))
    assert acc["evidence"]["not_stored"] == 2


def test_retrieval_summary_separates_write_misses_from_ranking_misses():
    idx = harness.index_sources([_src("a", "u1", ops=0), _src("b", "u2", ops=2)])
    acc = _fold(([], ["a", "b"], idx))
    text = "\n".join(harness.format_retrieval_summary(acc))
    assert "not_stored" in text
    assert "stored_not_retrieved" in text


def test_retrieval_summary_is_empty_when_nothing_was_scored():
    assert harness.format_retrieval_summary({}) == []


def test_retrieval_summary_flags_questions_it_could_not_score():
    """A question whose gold sessions are absent from the store is an
    ingest or labelling gap. Folding it in as recall 0 would read as a
    retrieval failure, so it is counted separately and shown."""
    idx = harness.index_sources([_src("s", "u1")])
    acc = _fold((["u1"], ["s"], idx))
    acc["unscored"] = 3
    text = "\n".join(harness.format_retrieval_summary(acc))
    assert "unscored" in text
    assert "3" in text


class _FakeAnamnesia:
    """Stands in for a live stack: serves a fixed source table and hit list."""

    def __init__(self, sources, hits, rows=None):
        self._sources = sources
        self._hits = hits
        self._rows = rows or {}
        self.retrieved = []

    def sources(self, *, user):
        return self._sources

    def browse(self, domain, *, user):
        return list(self._rows.get(domain, []))

    def retrieve(self, *, user, prompt, k=0):
        self.retrieved.append(prompt)
        return self._hits


def _run_retrieval(sources, hits, gold_sessions):
    anam = _FakeAnamnesia(sources, hits)
    q = {
        "question_id": "q1",
        "question_type": "multi-session",
        "question": "what did I graduate in?",
        "answer": "Graphic Design",
        "question_date": "2023/05/30 (Tue) 23:40",
        "haystack_sessions": [],
        "haystack_dates": [],
        "haystack_session_ids": [],
        "answer_session_ids": gold_sessions,
    }
    return harness.run_question(
        q,
        anam=anam,
        user_prefix="lme",
        ingest_wait=0,
        retrieve_k=5,
        multi_query=False,
        multi_query_total=40,
        skip_ingest=True,
        ingest_mode="extract",
        generate_provider="openai",
        generate_model="unused",
        judge_provider="openai",
        judge_model="unused",
        mode="retrieval",
    )


def test_retrieval_mode_scores_against_gold_without_calling_any_model(monkeypatch):
    """The whole point of the mode: no answerer, no judge, so a run costs
    nothing per question and carries none of their variance."""
    def explode(*a, **kw):
        raise AssertionError("retrieval mode must not call an LLM")

    monkeypatch.setattr(harness, "call_llm", explode)
    r = _run_retrieval(
        [_src("sess-a", "u1"), _src("sess-b", "u2")],
        [{"experience": {"source_id": "u1"}}],
        ["sess-a"],
    )
    assert r["score"]["recall"][1] == 1.0
    assert r["evidence"] == {"sess-a": "retrieved"}
    assert "predicted" not in r and "correct" not in r


def test_retrieval_mode_blames_the_write_path_when_evidence_was_never_stored():
    r = _run_retrieval(
        [_src("sess-a", "u1", ops=0, state="skipped")],
        [{"fact": {"source_id": "u9"}}],
        ["sess-a"],
    )
    assert r["score"]["recall"][5] == 0.0
    assert r["evidence"] == {"sess-a": "not_stored"}


def test_retrieval_mode_refuses_a_dataset_without_evidence_labels():
    """LoCoMo has no answer_session_ids. Scoring it silently would report
    recall 0 for every question."""
    try:
        _run_retrieval([_src("sess-a", "u1")], [], [])
    except ValueError as e:
        assert "answer_session_ids" in str(e)
        return
    raise AssertionError("expected ValueError")


# ---------- did the write path actually capture the answer? ------------------
#
# ops_produced > 0 only says a source produced rows, not that it produced
# rows carrying its answer. LongMemEval question 58bf7951 extracted 8 ops
# from the session that says "a production of The Glass Menagerie" and
# kept none of them about the play, which the old classifier reported as
# stored_not_retrieved: a ranking failure, when retrieval never had
# anything to rank.


def test_answer_terms_keeps_content_words():
    assert harness.answer_terms("The Glass Menagerie") == {"glass", "menagerie"}


def test_answer_terms_keeps_numbers():
    """Dates and counts are common answers and carry the whole meaning."""
    assert "1985" in harness.answer_terms("in 1985")


def test_answer_terms_survives_a_non_string_answer():
    """LongMemEval answers are not all strings: counts like 2, 4 and 1300
    appear as ints, and 3 of the 30-question subset are ints. Calling
    .lower() on one crashed the whole question."""
    assert harness.answer_terms(2) == set()
    assert "1300" in harness.answer_terms(1300)


def test_an_answer_with_no_usable_terms_falls_back_to_the_coarse_verdict():
    """A bare "2" yields no term worth searching for. Claiming the answer
    is missing because an unsearchable term was not found would blame the
    write path for a labelling limitation."""
    idx = harness.index_sources([_src("s", "u1", ops=3)])
    got = harness.classify_evidence(
        ["s"], idx, set(), terms=harness.answer_terms(2),
        source_text={"s": "unrelated"}, corpus_text="unrelated",
    )
    assert got == {"s": "stored_not_retrieved"}


def test_answer_terms_is_case_insensitive():
    assert harness.answer_terms("GLASS") == harness.answer_terms("glass")


def test_bears_answer_is_true_on_any_overlap():
    """Deliberately lenient: a claim that the write path dropped something
    must be hard to make, so any surviving content word counts as kept."""
    assert harness.bears_answer("user saw the menagerie play", {"glass", "menagerie"}) is True


def test_bears_answer_is_false_only_when_nothing_survived():
    assert harness.bears_answer("user likes hybrid bikes", {"glass", "menagerie"}) is False


def test_evidence_without_answer_terms_keeps_the_old_verdicts():
    """Capture analysis is optional; LoCoMo has no answer to check."""
    idx = harness.index_sources([_src("s", "u1", ops=3)])
    assert harness.classify_evidence(["s"], idx, set()) == {"s": "stored_not_retrieved"}


def test_rows_bearing_the_answer_that_did_not_rank_are_a_ranking_failure():
    idx = harness.index_sources([_src("s", "u1", ops=3)])
    got = harness.classify_evidence(
        ["s"], idx, set(), terms={"menagerie"},
        source_text={"s": "attended the menagerie production"},
        corpus_text="attended the menagerie production",
    )
    assert got == {"s": "stored_not_retrieved"}


def test_an_answer_stored_under_another_source_is_flagged_as_such():
    """Provenance moved: the content was captured, but not attributed to
    the session it came from, so a source-granularity label cannot find
    it. Distinct from extraction having dropped it."""
    idx = harness.index_sources([_src("s", "u1", ops=3)])
    rows = ["user likes hybrid bikes", "attended the menagerie production",
            "bought milk", "went running"]
    got = harness.classify_evidence(
        ["s"], idx, set(), terms={"menagerie"},
        source_text={"s": "user likes hybrid bikes"},
        corpus_text=" ".join(rows), rows=rows,
    )
    assert got == {"s": "answer_elsewhere"}


def test_an_answer_in_no_row_at_all_is_an_extraction_miss():
    idx = harness.index_sources([_src("s", "u1", ops=8)])
    rows = ["user likes hybrid bikes", "bought milk", "went running", "read a book"]
    got = harness.classify_evidence(
        ["s"], idx, set(), terms={"menagerie"},
        source_text={"s": "user likes hybrid bikes"},
        corpus_text=" ".join(rows), rows=rows,
    )
    assert got == {"s": "answer_missing"}


def test_a_retrieved_source_stays_retrieved_whatever_its_rows_say():
    """recall is what it is; capture analysis explains misses, not hits."""
    idx = harness.index_sources([_src("s", "u1", ops=3)])
    got = harness.classify_evidence(
        ["s"], idx, {"u1"}, terms={"menagerie"},
        source_text={"s": "nothing relevant"}, corpus_text="nothing relevant",
    )
    assert got == {"s": "retrieved"}


def test_a_source_with_no_rows_is_still_not_stored():
    idx = harness.index_sources([_src("s", "u1", ops=0)])
    got = harness.classify_evidence(
        ["s"], idx, set(), terms={"menagerie"}, source_text={}, corpus_text="",
    )
    assert got == {"s": "not_stored"}


# ---------- only distinctive terms are evidence ------------------------------
#
# "any content word matched" is too weak a test to carry a verdict. The
# 30-question baseline reported 6 answer_elsewhere verdicts, and five were
# artifacts: an abstention question matched on "any"/"did"/"not", a
# derived answer ("14 days") matched on "day"/"days"/"last", and a
# security answer matched "one"/"two"/"time" while missing "biometric",
# "otp" and "authentication" entirely.


def test_distinctive_terms_drop_words_that_are_everywhere():
    rows = ["a day at work", "another day", "day three", "day four"]
    assert harness.distinctive_terms({"day", "menagerie"}, rows) == {"menagerie"}


def test_distinctive_terms_keep_a_rare_word():
    rows = ["saw the glass menagerie", "bought milk", "went running", "read a book"]
    assert "menagerie" in harness.distinctive_terms({"menagerie"}, rows)


def test_distinctive_terms_of_an_empty_corpus_are_empty():
    """No rows means no basis to call anything rare."""
    assert harness.distinctive_terms({"menagerie"}, []) == set()


def test_abstention_questions_get_no_capture_verdict():
    """Their gold answer explains why the question is unanswerable, so
    there is nothing to look for in the store."""
    idx = harness.index_sources([_src("s", "u1", ops=3)])
    got = harness.classify_evidence(
        ["s"], idx, set(), terms={"museum", "december"},
        source_text={"s": "unrelated"}, corpus_text="a museum trip",
        rows=["a museum trip", "x", "y", "z"], abstention=True,
    )
    assert got == {"s": "stored_not_retrieved"}


def test_an_answer_whose_every_term_is_ubiquitous_gets_no_capture_verdict():
    """"14 days" reduces to words that are in every row. There is nothing
    left to look for, so claiming it was captured elsewhere, or dropped,
    would both be inventions."""
    rows = ["it took days", "several days later", "days passed", "many days"]
    got = harness.classify_evidence(
        ["s"], harness.index_sources([_src("s", "u1", ops=3)]), set(),
        terms={"days"}, source_text={"s": "nope"},
        corpus_text=" ".join(rows), rows=rows,
    )
    assert got == {"s": "stored_not_retrieved"}


def test_grading_asides_are_not_answer_content():
    """Gold answers carry instructions to the judge. "15 days is also
    acceptable" must not contribute searchable terms."""
    terms = harness.answer_terms("14 days. 15 days (including the last day) is also acceptable.")
    # "14" and "15" are shorter than the minimum term length, so nothing
    # searchable survives at all, which is the honest outcome for an
    # answer that is a computed number.
    assert terms == set(), terms


def test_a_distinctive_term_found_under_another_source_is_still_answer_elsewhere():
    rows = ["book club page turners", "bought milk", "went running", "read a book"]
    got = harness.classify_evidence(
        ["s"], harness.index_sources([_src("s", "u1", ops=3)]), set(),
        terms={"turners"}, source_text={"s": "bought milk"},
        corpus_text=" ".join(rows), rows=rows,
    )
    assert got == {"s": "answer_elsewhere"}


def test_a_distinctive_term_in_no_row_is_still_answer_missing():
    rows = ["bought milk", "went running", "read a book", "cooked dinner"]
    got = harness.classify_evidence(
        ["s"], harness.index_sources([_src("s", "u1", ops=3)]), set(),
        terms={"menagerie"}, source_text={"s": "bought milk"},
        corpus_text=" ".join(rows), rows=rows,
    )
    assert got == {"s": "answer_missing"}


def test_row_text_groups_facts_and_experiences_by_source():
    by_src, corpus, _ = harness.index_row_text(
        [{"key": "user.play", "value": {"v": "Glass Menagerie"}, "source_id": "u1"}],
        [{"title": "Bike ride", "body": "rode a hybrid bike", "source_id": "u2"}],
    )
    assert "menagerie" in by_src["u1"].lower()
    assert "hybrid bike" in by_src["u2"]
    assert "menagerie" in corpus.lower() and "hybrid bike" in corpus


def test_row_text_includes_the_fact_key_not_just_the_value():
    """Keys carry meaning the extractor put nowhere else, e.g.
    user.recent_audition.play."""
    by_src, _, _ = harness.index_row_text(
        [{"key": "user.recent_audition.play", "value": {"v": "x"}, "source_id": "u1"}], []
    )
    assert "audition" in by_src["u1"]


def test_row_text_keeps_rows_with_no_source_in_the_corpus():
    """A row whose provenance was lost still proves the content was
    captured, which is the difference between answer_elsewhere and
    answer_missing."""
    _, corpus, _ = harness.index_row_text(
        [], [{"title": "t", "body": "orphaned content", "source_id": None}]
    )
    assert "orphaned content" in corpus


# ---------- graph sources ----------------------------------------------------
#
# Extractor.Run only dispatches to the graph pass for sources of kind
# "claude-session-graph". Ingesting sessions alone leaves the graph empty
# however graph.extract is set, which reads as "the graph contributes
# nothing" when it never ran. The hook posts an extra graph source per
# checkpoint carrying segment_source_ids; without those, entity mentions
# land only on the graph source, which no search hit carries.


class _RecordingAnamnesia:
    def __init__(self, hits=(), sources=(), rows=None):
        self.ingested = []
        self._hits = list(hits)
        self._sources = list(sources)
        self._rows = rows or {}

    def ingest(self, *, user, content, kind="longmemeval-session",
               occurred_at=None, external_ref=None, metadata=None):
        sid = f"src-{len(self.ingested)}"
        self.ingested.append(
            {"kind": kind, "content": content, "external_ref": external_ref,
             "metadata": metadata, "occurred_at": occurred_at, "_id": sid}
        )
        return {"source_id": sid}

    def wait_for_queue_drain(self, *, user, timeout_s, **kw):
        return {"extract_pending": 0, "embed_pending": 0}

    def retrieve(self, *, user, prompt, k=0):
        return self._hits

    def sources(self, *, user):
        return self._sources

    def browse(self, domain, *, user):
        return list(self._rows.get(domain, []))


def _run_ingest(graph_source):
    anam = _RecordingAnamnesia(
        hits=[{"fact": {"source_id": "u1"}}], sources=[_src("s-a", "u1")]
    )
    q = {
        "question_id": "q1",
        "question_type": "multi-session",
        "question": "q?",
        "answer": "a",
        "question_date": "2023/05/30 (Tue) 23:40",
        "haystack_sessions": [
            [{"role": "user", "content": "first session text"}],
            [{"role": "user", "content": "second session text"}],
        ],
        "haystack_dates": ["2023/05/20 (Sat) 02:21", "2023/05/21 (Sun) 03:00"],
        "haystack_session_ids": ["s-a", "s-b"],
        "answer_session_ids": ["s-a"],
    }
    harness.run_question(
        q, anam=anam, user_prefix="lme", ingest_wait=1, retrieve_k=5,
        multi_query=False, multi_query_total=40, skip_ingest=False,
        ingest_mode="extract", generate_provider="openai", generate_model="x",
        judge_provider="openai", judge_model="x", mode="retrieval",
        graph_source=graph_source,
    )
    return anam


def test_a_graph_source_is_posted_for_each_session():
    anam = _run_ingest(True)
    kinds = [i["kind"] for i in anam.ingested]
    assert kinds.count("longmemeval-session") == 2
    assert kinds.count("claude-session-graph") == 2


def test_no_graph_source_is_posted_when_disabled():
    anam = _run_ingest(False)
    assert all(i["kind"] == "longmemeval-session" for i in anam.ingested)


def test_the_graph_source_points_at_the_session_it_came_from():
    """Without segment_source_ids the extractor logs that mentions will
    land only on the graph source, which no search hit carries, so the
    graph channel can never map back to a retrievable row."""
    anam = _run_ingest(True)
    sessions = [i for i in anam.ingested if i["kind"] == "longmemeval-session"]
    graphs = [i for i in anam.ingested if i["kind"] == "claude-session-graph"]
    for sess, graph in zip(sessions, graphs):
        assert graph["metadata"]["segment_source_ids"] == [sess["_id"]]


def test_the_graph_source_carries_the_session_text():
    anam = _run_ingest(True)
    graphs = [i for i in anam.ingested if i["kind"] == "claude-session-graph"]
    assert "first session text" in graphs[0]["content"]
    assert "second session text" in graphs[1]["content"]


def test_the_graph_source_keeps_the_session_date():
    """Edges are bitemporal; a graph source dated 'now' would put every
    benchmark relationship at run time instead of when it happened."""
    anam = _run_ingest(True)
    sessions = [i for i in anam.ingested if i["kind"] == "longmemeval-session"]
    graphs = [i for i in anam.ingested if i["kind"] == "claude-session-graph"]
    for sess, graph in zip(sessions, graphs):
        assert graph["occurred_at"] == sess["occurred_at"]


# ---------- mirroring the server's graph setting -----------------------------


def test_config_flags_reads_the_servers_effective_settings():
    flags = harness.config_flags(
        [{"key": "graph.extract", "value": "true"},
         {"key": "llm.model", "value": "openai/gpt-4o-mini"}]
    )
    assert flags["graph.extract"] == "true"


def test_graph_extraction_is_detected_as_on():
    assert harness.graph_enabled({"graph.extract": "true"}) is True


def test_graph_extraction_is_detected_as_off():
    assert harness.graph_enabled({"graph.extract": "false"}) is False
    assert harness.graph_enabled({}) is False


def test_retrieval_summary_stays_quiet_when_everything_scored():
    idx = harness.index_sources([_src("s", "u1")])
    text = "\n".join(harness.format_retrieval_summary(_fold((["u1"], ["s"], idx))))
    assert "unscored" not in text
