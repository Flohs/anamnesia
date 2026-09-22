// recall.go decides whether a retrieval actually recalled anything, and
// adds it to the tally.
//
// The count exists because nothing else could answer "has this been
// working". The activity recorder holds retrievals in memory only, so a
// restart erased the evidence, and the row counts say what is stored
// rather than what came back.
//
// What it must not do is flatter itself. A retrieval returns the top K
// rows whether or not any of them match, so "it returned something" is a
// statement about the corpus being non-empty. Only two numbers here are
// absolute: the reranker's relevance score, and the cosine distance the
// vector channel ranked by. Where neither exists, the outcome is
// recorded as ungraded rather than counted as a success.
package httpapi

import (
	"context"

	"github.com/google/uuid"

	"github.com/flohs/anamnesia/internal/store"
	"github.com/flohs/anamnesia/pkg/anamnesia"
)

// recallBars is what a retrieval is judged against.
//
// TrustDistance is false where the embedder is a stub, which hashes text
// into a random unit vector: a prompt and the memory that answers it end
// up about as far apart as two unrelated texts, so its distances are not
// evidence of anything. That is the default install, and grading it
// would report a permanent nothing-recalled caused by configuration
// rather than by retrieval.
type recallBars struct {
	MinScore      float64
	MaxDistance   float64
	TrustDistance bool
}

// recallBars reads the bars this server judges by.
func (d Deps) recallBars() recallBars {
	return recallBars{
		MinScore:      d.RecallMinScore,
		MaxDistance:   d.RecallMaxDistance,
		TrustDistance: d.EmbedProvider != "stub",
	}
}

// gradeRecall judges one retrieval's hits against the absolute bars.
//
// The reranker wins where it ran: it scored these exact hits against
// this exact query, which is a better judgement than the distance
// between two embeddings, and it stays a judgement even where the
// embeddings are not one. Only when nothing was reranked does the
// distance decide.
func gradeRecall(hits []anamnesia.SearchHit, bars recallBars) store.RecallOutcome {
	if len(hits) == 0 {
		return store.RecallMiss
	}
	reranked := false
	for _, h := range hits {
		if h.RerankerRank == 0 {
			continue
		}
		reranked = true
		if h.Score >= bars.MinScore {
			return store.RecallHit
		}
	}
	if reranked {
		return store.RecallMiss
	}
	if !bars.TrustDistance {
		return store.RecallUngraded
	}
	measured := false
	for _, h := range hits {
		if h.Distance == nil {
			continue
		}
		measured = true
		if *h.Distance <= bars.MaxDistance {
			return store.RecallHit
		}
	}
	if measured {
		return store.RecallMiss
	}
	return store.RecallUngraded
}

// recordRecall adds one retrieval to the tally, and never lets that
// failing cost the caller anything. A session must not lose its memory
// because a counter could not be written, which is the same rule
// artifact lookup already follows.
func (d Deps) recordRecall(ctx context.Context, userID uuid.UUID, outcome store.RecallOutcome) {
	if d.Store == nil {
		return
	}
	if err := d.Store.RecordRetrieval(ctx, userID, outcome); err != nil {
		if d.Log != nil {
			d.Log.Warn("could not record the recall tally", "err", err)
		}
	}
}

// recallWindowDays is how far back the console's tally reaches.
//
// Short enough that changing the embedding model, or an outage, shows up
// within a week rather than being diluted by months of history. The rows
// themselves are kept indefinitely, so a longer view costs a query
// rather than a migration.
const recallWindowDays = 7
