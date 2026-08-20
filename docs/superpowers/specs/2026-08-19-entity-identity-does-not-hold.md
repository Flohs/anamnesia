# Finding: the graph's value rests on entity identity, which does not hold

Written 2026-08-19, from an end-to-end run of the graph work on
`feat/graph-extraction` after the bridge fix. **Not a defect in that branch.**
The plumbing works; this is about whether it will be worth anything.

---

## What happened

Two real sessions through the real hook, real model, `graph.extract` on. The
same person is discussed in both. The graph came out as two disconnected
subgraphs:

```
priha-raman  -[owns]->    nightly-stock-reconciliation-job
priha-raman  -[owns]->    warehouse-settlement-service
priha-raman  -[owns]->    sku-catalog
priha-raman  -[owns]->    thursday-inventory-review

dana-okafor  -[covers]->  priya-raman
```

The model wrote "Priya Raman" as `priha-raman` in the first session and
`priya-raman` in the second. Two nodes, one person. No walk can cross between
them, so the second session's knowledge is unreachable from the first's — which
is precisely the connection the graph exists to make.

`normaliseEntityName` lowercases, trims and collapses whitespace. It cannot fix
a typo, and nothing else tries to.

## Why this matters more than it looks

A graph is only worth its edges, and an edge is only worth anything if both
endpoints are the entity you meant. Entity identity here is **exact string
equality on a model-produced name**, enforced by a unique index on
`(scope, kind, name)`. Every session is an independent chance for the model to
spell a name differently, and every difference silently forks a node.

The failure is invisible in every direction:

- **No test catches it.** Every test constructs its own entity names, so they
  are consistent by construction. The system's inconsistency is exactly what a
  hand-written fixture cannot reproduce.
- **No error surfaces.** Two nodes is a valid graph. The trace records both
  upserts as successes.
- **It compounds.** Each new session adds nodes to whichever spelling it
  happens to produce, so the subgraphs drift further apart over time rather
  than converging.
- **It is worst where the graph is most valuable.** Long-lived entities
  discussed across many sessions — a person, a service, a project — get the
  most chances to fork.

## What would actually fix it

**Embedding-similarity resolution on write.** `entities.embedding` exists,
`entities_embedding` is an HNSW index that every dims migration rebuilds, and
the column is permanently NULL. Embedding a new entity's name and merging it
into an existing entity above a similarity threshold is the standard answer,
and `priha-raman` against `priya-raman` is exactly the case it handles that
exact matching cannot.

This was deliberately scoped out of the graph branch, on the grounds that doing
both at once would confuse "the graph helps" with "entity search helps". That
reasoning still stands for measurement. It does not stand as a reason to ship
the graph and expect value from it: on this evidence the graph will fragment
faster than it accumulates.

Two lesser options, worth naming so the choice is explicit:

- **Give the model the existing entity names for the scope** in the graph
  prompt, as candidates to match against, the way the fact pass is already
  given candidate memories. Cheaper than embeddings, and it makes the model's
  own consistency the mechanism rather than a coincidence.
- **Merge on write by edit distance.** Crude, fast, and wrong for genuinely
  similar-but-distinct names (`checkout-service` vs `checkout-services`).

## The honest position on the branch

After the bridge fix, an end-to-end run gives:

- entity_mentions correctly spanning segment sources — 15 rows across 3 sources
  where the defect gave 5 across 1
- the seed join returning 5 entities where it returned 0 by construction
- the channel firing on a live query, one hit carrying `GraphRank`
- **but zero hits reachable only via the graph**, because the two sessions that
  should have been connected were not

So: the mechanism is proven and its usefulness is not. Whether the graph earns
one model call per checkpoint is now a question about entity resolution, and it
cannot be answered until that is stronger than exact string equality.
