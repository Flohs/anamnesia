package httpapi

import (
	"math"
	"testing"
)

func TestPCAFindsTheDominantAxis(t *testing.T) {
	// Four points on a straight line through 3-space. One component
	// explains all of it, and the second explains none.
	coords, explained := pca2([][]float32{
		{0, 0, 0}, {1, 1, 0}, {2, 2, 0}, {3, 3, 0},
	})
	if len(coords) != 4 {
		t.Fatalf("coords = %d, want 4", len(coords))
	}
	if explained[0] < 0.99 {
		t.Errorf("first component explains %.3f, want nearly all of it", explained[0])
	}
	if explained[1] > 0.01 {
		t.Errorf("second component explains %.3f, want nearly none", explained[1])
	}
	// Collinear points must come out ordered along the axis, whichever
	// direction the component happens to point in.
	forward := coords[0][0] < coords[1][0] && coords[1][0] < coords[2][0] && coords[2][0] < coords[3][0]
	backward := coords[0][0] > coords[1][0] && coords[1][0] > coords[2][0] && coords[2][0] > coords[3][0]
	if !forward && !backward {
		t.Errorf("x coordinates %v are not monotonic along a straight line", coords)
	}
}

func TestPCASeparatesTwoAxes(t *testing.T) {
	// A wide, short cloud: most variance along x, some along y, none on z.
	coords, explained := pca2([][]float32{
		{-10, -1, 0}, {-10, 1, 0}, {10, -1, 0}, {10, 1, 0},
	})
	if explained[0] < 0.9 || explained[1] > 0.1 {
		t.Errorf("explained variance = %v, want the wide axis to dominate", explained)
	}
	if math.Abs(coords[0][0]-coords[1][0]) > 1e-6 {
		t.Errorf("the two points sharing an x should project together: %v", coords[:2])
	}
	if explained[0]+explained[1] > 1.0000001 {
		t.Errorf("explained variance sums to %.6f, which is more than all of it",
			explained[0]+explained[1])
	}
}

func TestPCAOnTooFewPoints(t *testing.T) {
	// One point has no variance to explain, and must not divide by zero.
	coords, explained := pca2([][]float32{{1, 2, 3}})
	if len(coords) != 1 {
		t.Fatalf("coords = %v", coords)
	}
	if explained[0] != 0 || explained[1] != 0 {
		t.Errorf("explained = %v, want zeroes rather than a fabricated structure", explained)
	}
	if coords, explained := pca2(nil); len(coords) != 0 || len(explained) != 2 {
		t.Errorf("empty input = %v %v", coords, explained)
	}
}
