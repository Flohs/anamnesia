// eval_score.go — retrieval metrics.
//
// Relevance is at source granularity: a source produces an unpredictable
// number of rows, so labelling row ids would make the gold set a hostage
// of the extraction prompt. A hit counts when its source_id is labelled.
package main

import "sort"

type queryScore struct {
	ID          string
	RecallAt    map[int]float64
	PrecisionAt map[int]float64
	MRR         float64
	Found       bool
	LatencyMS   int64
}

type aggregateScore struct {
	Queries     int
	RecallAt    map[int]float64
	PrecisionAt map[int]float64
	MRR         float64
	ZeroHit     int
	P50MS       int64
	P95MS       int64
}

// score measures one query. ranked is source ids in rank order and may
// repeat: several rows can come from one source. Recall counts distinct
// sources, so duplicates collapse to their first appearance.
func score(id string, ranked, relevant []string, ks []int, latencyMS int64) queryScore {
	want := make(map[string]bool, len(relevant))
	for _, r := range relevant {
		want[r] = true
	}
	seen := make(map[string]bool, len(ranked))
	distinct := make([]string, 0, len(ranked))
	for _, s := range ranked {
		if seen[s] {
			continue
		}
		seen[s] = true
		distinct = append(distinct, s)
	}

	s := queryScore{
		ID:          id,
		RecallAt:    make(map[int]float64, len(ks)),
		PrecisionAt: make(map[int]float64, len(ks)),
		LatencyMS:   latencyMS,
	}
	for _, k := range ks {
		hits := 0
		n := min(k, len(distinct))
		for _, src := range distinct[:n] {
			if want[src] {
				hits++
			}
		}
		if len(want) > 0 {
			s.RecallAt[k] = float64(hits) / float64(len(want))
		}
		if k > 0 {
			s.PrecisionAt[k] = float64(hits) / float64(k)
		}
	}
	for i, src := range distinct {
		if want[src] {
			s.MRR = 1.0 / float64(i+1)
			s.Found = true
			break
		}
	}
	return s
}

// aggregate means the per-query numbers and reports the two that matter
// on their own: how many queries found nothing, and how slow the slow
// ones were.
func aggregate(scores []queryScore, ks []int) aggregateScore {
	agg := aggregateScore{
		Queries:     len(scores),
		RecallAt:    make(map[int]float64, len(ks)),
		PrecisionAt: make(map[int]float64, len(ks)),
	}
	if len(scores) == 0 {
		return agg
	}
	lat := make([]int64, 0, len(scores))
	for _, s := range scores {
		for _, k := range ks {
			agg.RecallAt[k] += s.RecallAt[k]
			agg.PrecisionAt[k] += s.PrecisionAt[k]
		}
		agg.MRR += s.MRR
		if !s.Found {
			agg.ZeroHit++
		}
		lat = append(lat, s.LatencyMS)
	}
	n := float64(len(scores))
	for _, k := range ks {
		agg.RecallAt[k] /= n
		agg.PrecisionAt[k] /= n
	}
	agg.MRR /= n
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	agg.P50MS = percentile(lat, 0.50)
	agg.P95MS = percentile(lat, 0.95)
	return agg
}

// percentile takes the nearest-rank value from a sorted slice.
func percentile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)-1))
	return sorted[i]
}
