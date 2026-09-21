package extract

import (
	"context"
	"testing"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func graphSource(content string) *anamnesia.Source {
	src := testSource(content)
	src.Kind = graphSourceKind
	return src
}

// Run reports whether the model ran, because the worker turns that into the
// sources row state. A gate skip and a model that looked and found nothing
// are both zero operations, and recording them as the same state is what
// cost us the ability to ask how often the model is paid to say nothing.
func TestRunReportsWhetherTheModelWasCalled(t *testing.T) {
	const long = "Some content comfortably longer than the minimum length."

	cases := []struct {
		name   string
		source *anamnesia.Source
		want   bool
	}{
		{"content below the minimum", testSource("short"), false},
		{"graph pass switched off", graphSource(long), false},
		{"the model looked and found nothing", testSource(long), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := &fakeLLM{Ops: []Operation{{Op: "NOOP"}}}
			ex := &Extractor{LLM: fake}

			out, err := ex.Run(context.Background(), c.source)
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			if out.ModelCalled != c.want {
				t.Errorf("ModelCalled = %v, want %v", out.ModelCalled, c.want)
			}
			// The fake counts calls, so what Run reports is checked against
			// what actually happened rather than against itself.
			if actually := fake.Calls > 0; actually != out.ModelCalled {
				t.Errorf("reported ModelCalled = %v, but the model was called %d times",
					out.ModelCalled, fake.Calls)
			}
			if out.Ops != 0 {
				t.Errorf("Ops = %d, want 0", out.Ops)
			}
		})
	}
}
