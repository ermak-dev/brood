package pipeline

import (
	"fmt"
	"testing"
)

func BenchmarkValidationChain(b *testing.B) {
	for _, size := range []int{1000, 5000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			d := &Document{Name: "bench", Start: "n0", Exit: fmt.Sprintf("n%d", size-1), Nodes: make(map[string]Node), GoalGates: []string{"n1"}}
			for i := 0; i < size; i++ {
				name := fmt.Sprintf("n%d", i)
				d.Nodes[name] = Node{Type: "actor"}
				if i > 0 {
					d.Edges = append(d.Edges, Edge{From: fmt.Sprintf("n%d", i-1), To: name})
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := d.Validate(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
