package pipeline

import (
	"fmt"
	"testing"
	"time"
)

// BUG: Schema validation scales quadratically with actors and style defaults.
// HYPOTHESIS: Each actor scans every style when validating its source.
// SOLUTION: Source validation no longer resolves style defaults for each actor.
// RESULT: FIXED — validation does not scan styles per actor.
// STATUS: FIXED
func TestStyleDefaultsScaleLinearly(t *testing.T) {
	elapsed := func(n int) time.Duration {
		d := &Document{Name: "test", Nodes: make(map[string]Node, n)}
		for i := 0; i < n; i++ {
			d.Nodes[fmt.Sprintf("actor-%d", i)] = Node{Type: "actor"}
			d.Styles = append(d.Styles, Style{Selectors: []string{"type:actor"}, Template: "worker"})
		}
		best := time.Duration(1<<63 - 1)
		for i := 0; i < 3; i++ {
			start := time.Now()
			for repeat := 0; repeat < 20; repeat++ {
				if err := d.checkSchema("test"); err != nil {
					t.Fatal(err)
				}
			}
			if duration := time.Since(start); duration < best {
				best = duration
			}
		}
		return best
	}
	small, large := elapsed(1000), elapsed(4000)
	t.Logf("validation time: 1000 actors %s, 4000 actors %s", small, large)
	if large > small*9 {
		t.Fatalf("quadrupling actors and styles took %s vs %s (want at most 9x)", large, small)
	}
}
