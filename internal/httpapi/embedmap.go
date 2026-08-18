// embedmap.go projects stored embeddings onto a plane so they can be
// looked at.
//
// Two components by power iteration with deflation, over mean-centred
// vectors. That is the whole of it: no dependency, no solver, and it
// runs on demand over at most a couple of thousand rows.
//
// The projection reports how much variance it kept, and the UI is
// expected to show that. Two components out of 1536 can make unrelated
// memories look adjacent, and a scatter plot that does not say how much
// structure it is discarding invites exactly that misreading.
package httpapi

import (
	"math"
	"net/http"
	"strconv"

	"github.com/flohs/anamnesia/internal/store"
)

// pcaIterations is how many power-iteration passes each component gets.
// Well past convergence for this data, and still trivial next to the
// query that fetched the vectors.
const pcaIterations = 40

type embedPoint struct {
	ID      string  `json:"id"`
	Title   string  `json:"title"`
	Kind    string  `json:"kind,omitempty"`
	Project *string `json:"project"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
}

type embedMapResponse struct {
	Domain            string       `json:"domain"`
	N                 int          `json:"n"`
	Dims              int          `json:"dims"`
	ExplainedVariance []float64    `json:"explained_variance"`
	Points            []embedPoint `json:"points"`
}

func (d Deps) handleEmbeddingMap(w http.ResponseWriter, r *http.Request) {
	rs, ok := d.resolveReadScope(w, r)
	if !ok {
		return
	}
	domain := r.URL.Query().Get("domain")
	if domain == "" {
		domain = "experiences"
	}
	limit := 500
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "limit must be a positive number", http.StatusBadRequest)
			return
		}
		limit = n
	}
	sample, err := d.Store.EmbeddingSample(r.Context(), domain, rs.Scope, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	vectors := make([][]float32, len(sample))
	for i, row := range sample {
		vectors[i] = row.Vector
	}
	coords, explained := pca2(vectors)

	resp := embedMapResponse{
		Domain:            domain,
		N:                 len(sample),
		ExplainedVariance: explained,
		Points:            make([]embedPoint, 0, len(sample)),
	}
	if len(sample) > 0 {
		resp.Dims = len(sample[0].Vector)
	}
	for i, row := range sample {
		resp.Points = append(resp.Points, embedPoint{
			ID:      row.ID.String(),
			Title:   row.Title,
			Kind:    row.Kind,
			Project: row.Project,
			X:       coords[i][0],
			Y:       coords[i][1],
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

type activityBucketView struct {
	Date        string  `json:"date"`
	Project     *string `json:"project"`
	Sources     int     `json:"sources"`
	Facts       int     `json:"facts"`
	Experiences int     `json:"experiences"`
}

func (d Deps) handleActivityBuckets(w http.ResponseWriter, r *http.Request) {
	rs, ok := d.resolveReadScope(w, r)
	if !ok {
		return
	}
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "days must be a positive number", http.StatusBadRequest)
			return
		}
		days = n
	}
	buckets, err := d.Store.ActivityBuckets(r.Context(), rs.Scope, days)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	items := make([]activityBucketView, 0, len(buckets))
	for _, b := range buckets {
		items = append(items, activityBucketView(b))
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": items})
}

// pca2 projects vectors onto their two principal components and reports
// the fraction of total variance each one carries.
//
// Fewer than two points, or no variance at all, yields zero coordinates
// and zero explained variance: a projection of nothing is not a
// projection, and inventing an axis for it would be a lie the plot would
// then draw.
func pca2(vectors [][]float32) ([][2]float64, []float64) {
	n := len(vectors)
	coords := make([][2]float64, n)
	explained := []float64{0, 0}
	if n == 0 || len(vectors[0]) == 0 {
		return coords, explained
	}
	dims := len(vectors[0])

	// Mean-centre. PCA without centring finds the direction of the data
	// from the origin, which for embeddings is nearly the same direction
	// for everything.
	mean := make([]float64, dims)
	for _, v := range vectors {
		for j := 0; j < dims && j < len(v); j++ {
			mean[j] += float64(v[j])
		}
	}
	for j := range mean {
		mean[j] /= float64(n)
	}
	x := make([][]float64, n)
	total := 0.0
	for i, v := range vectors {
		row := make([]float64, dims)
		for j := 0; j < dims && j < len(v); j++ {
			row[j] = float64(v[j]) - mean[j]
			total += row[j] * row[j]
		}
		x[i] = row
	}
	if total == 0 {
		return coords, explained
	}

	for component := 0; component < 2; component++ {
		axis := dominantAxis(x)
		if axis == nil {
			break
		}
		energy := 0.0
		for i := range x {
			p := dot(x[i], axis)
			coords[i][component] = p
			energy += p * p
		}
		explained[component] = energy / total
		// Deflate: remove what this component explained, so the next
		// pass finds the largest of what is left.
		for i := range x {
			p := coords[i][component]
			for j := range x[i] {
				x[i][j] -= p * axis[j]
			}
		}
	}
	return coords, explained
}

// dominantAxis is the leading eigenvector of the covariance, by power
// iteration. Started from the longest row rather than a random vector,
// so the same data always produces the same plot.
func dominantAxis(x [][]float64) []float64 {
	if len(x) == 0 {
		return nil
	}
	dims := len(x[0])
	seed, best := -1, 0.0
	for i := range x {
		if norm := dot(x[i], x[i]); norm > best {
			seed, best = i, norm
		}
	}
	if seed < 0 || best == 0 {
		return nil
	}
	v := make([]float64, dims)
	copy(v, x[seed])
	normalise(v)

	next := make([]float64, dims)
	projections := make([]float64, len(x))
	for iteration := 0; iteration < pcaIterations; iteration++ {
		for i := range x {
			projections[i] = dot(x[i], v)
		}
		for j := range next {
			next[j] = 0
		}
		for i := range x {
			p := projections[i]
			if p == 0 {
				continue
			}
			for j := range x[i] {
				next[j] += p * x[i][j]
			}
		}
		if !normalise(next) {
			break
		}
		copy(v, next)
	}
	return v
}

func dot(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// normalise scales a vector to unit length, reporting false when there
// is nothing to scale.
func normalise(v []float64) bool {
	length := math.Sqrt(dot(v, v))
	if length == 0 || math.IsNaN(length) || math.IsInf(length, 0) {
		return false
	}
	for i := range v {
		v[i] /= length
	}
	return true
}

// compile-time proof the view mirrors the store type field for field.
var _ = func(b store.ActivityBucket) activityBucketView { return activityBucketView(b) }
