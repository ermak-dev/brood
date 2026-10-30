package substrate

import (
	"reflect"
	"strings"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/protobuf/proto"
)

// FEATURE: Чистая диагностика видимых причин отказа placement.
// SPEC: Оба селектора обязательны; свободные ресурсы, регистрация и состояние
// проверяются по SDK pin; отсутствие причин не обещает успешного размещения.
// IMPLEMENTATION: Добавлены typed-причины и административный formatter без RPC.
// RESULT: IMPLEMENTED — focused/race, полный Go build/test/vet и Python/ruff зелёные.
// STATUS: DONE
func placementWorker() *ateapi.Worker {
	return &ateapi.Worker{
		Metadata: &ateapi.ResourceMetadata{Name: "worker-a"},
		Labels:   map[string]string{"pool": "small", "empty": ""}, NodeName: "node-a",
		Status: &ateapi.WorkerStatus{
			State: ateapi.WorkerState_WORKER_STATE_ACTIVE, RegisteredEpoch: 1,
			SandboxClasses: []*ateapi.WorkerSandboxClass{{SandboxClass: "microvm"}},
			Capacity:       &ateapi.WorkerResources{CpuMillicores: 4000, MemoryBytes: 8192, Actors: 3},
			Allocated:      &ateapi.WorkerResources{CpuMillicores: 3000, MemoryBytes: 4096, Actors: 2},
		},
	}
}

func TestDiagnosePlacement(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ateapi.Worker, *PlacementConstraints)
		want   []PlacementReasonCode
	}{
		{name: "exact available capacity", want: nil},
		{name: "CPU", change: func(w *ateapi.Worker, c *PlacementConstraints) { c.Limits.CpuMillicores++ }, want: []PlacementReasonCode{PlacementCPU}},
		{name: "memory", change: func(w *ateapi.Worker, c *PlacementConstraints) { c.Limits.MemoryBytes++ }, want: []PlacementReasonCode{PlacementMemory}},
		{name: "slots", change: func(w *ateapi.Worker, c *PlacementConstraints) { w.Status.Allocated.Actors = 3 }, want: []PlacementReasonCode{PlacementActorSlots}},
		{name: "draining", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			w.Status.State = ateapi.WorkerState_WORKER_STATE_DRAINING
		}, want: []PlacementReasonCode{PlacementInactive}},
		{name: "unavailable", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			w.Status.State = ateapi.WorkerState_WORKER_STATE_UNAVAILABLE
		}, want: []PlacementReasonCode{PlacementInactive}},
		{name: "epoch", change: func(w *ateapi.Worker, c *PlacementConstraints) { w.Status.RegisteredEpoch = 0 }, want: []PlacementReasonCode{PlacementRegistration}},
		{name: "class registration", change: func(w *ateapi.Worker, c *PlacementConstraints) { w.Status.SandboxClasses = nil }, want: []PlacementReasonCode{PlacementRegistration}},
		{name: "template selector", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			c.TemplateSelector = &ateapi.Selector{MatchLabels: map[string]string{"pool": "large"}}
		}, want: []PlacementReasonCode{PlacementTemplateSelector}},
		{name: "actor selector is AND", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			c.ActorSelector = &ateapi.Selector{MatchLabels: map[string]string{"pool": "large"}}
		}, want: []PlacementReasonCode{PlacementActorSelector}},
		{name: "missing label is not empty", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			c.ActorSelector = &ateapi.Selector{MatchLabels: map[string]string{"missing": ""}}
		}, want: []PlacementReasonCode{PlacementActorSelector}},
		{name: "sandbox class", change: func(w *ateapi.Worker, c *PlacementConstraints) { c.SandboxClass = "gvisor" }, want: []PlacementReasonCode{PlacementSandboxClass}},
		{name: "required node", change: func(w *ateapi.Worker, c *PlacementConstraints) { c.RequiredNodes = []string{"node-b"} }, want: []PlacementReasonCode{PlacementNode}},
		{name: "nil limits", change: func(w *ateapi.Worker, c *PlacementConstraints) { c.Limits = nil }, want: nil},
		{name: "zero compute capacity is zero", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			w.Status.Capacity.CpuMillicores = 0
			w.Status.Capacity.MemoryBytes = 0
		}, want: []PlacementReasonCode{PlacementCPU, PlacementMemory}},
		{name: "zero request bypasses overcommitted compute", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			c.Limits = &ateapi.Resources{}
			w.Status.Capacity.CpuMillicores = 0
		}, want: nil},
		{name: "one zero request still compares both dimensions", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			c.Limits.CpuMillicores = 0
			w.Status.Capacity.CpuMillicores = 0
		}, want: []PlacementReasonCode{PlacementCPU}},
		{name: "all reasons retained", change: func(w *ateapi.Worker, c *PlacementConstraints) {
			w.Status.RegisteredEpoch = 0
			w.Status.State = ateapi.WorkerState_WORKER_STATE_DRAINING
			w.Status.Allocated.Actors = 3
			c.Limits.CpuMillicores = 1001
			c.Limits.MemoryBytes = 4097
		}, want: []PlacementReasonCode{PlacementRegistration, PlacementInactive, PlacementActorSlots, PlacementCPU, PlacementMemory}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := placementWorker()
			c := PlacementConstraints{Limits: &ateapi.Resources{CpuMillicores: 1000, MemoryBytes: 4096}, TemplateSelector: &ateapi.Selector{MatchLabels: map[string]string{"pool": "small"}}}
			if tt.change != nil {
				tt.change(w, &c)
			}
			before := proto.Clone(w)
			limits := proto.Clone(c.Limits)
			selector := proto.Clone(c.TemplateSelector)
			got := DiagnosePlacement([]*ateapi.Worker{w}, c)
			if len(got.Workers) != 1 || got.Workers[0].Name != "worker-a" {
				t.Fatalf("workers: %+v", got)
			}
			var codes []PlacementReasonCode
			for _, r := range got.Workers[0].Reasons {
				codes = append(codes, r.Code)
			}
			if !reflect.DeepEqual(codes, tt.want) {
				t.Fatalf("reasons = %v, want %v", codes, tt.want)
			}
			if got.Unchecked == "" {
				t.Fatal("missing honest unknown-constraints caveat")
			}
			if !proto.Equal(before, w) || !proto.Equal(limits, c.Limits) || !proto.Equal(selector, c.TemplateSelector) {
				t.Fatal("mutated input")
			}
		})
	}
}

func TestPlacementEmptyAndMissing(t *testing.T) {
	for _, workers := range [][]*ateapi.Worker{nil, {}} {
		got := DiagnosePlacement(workers, PlacementConstraints{})
		if !got.NoWorkers || len(got.Workers) != 0 || !strings.Contains(got.String(), "no workers in supplied snapshot") {
			t.Fatalf("empty: %+v", got)
		}
	}
	got := DiagnosePlacement([]*ateapi.Worker{nil}, PlacementConstraints{})
	if got.NoWorkers || len(got.Workers) != 1 || got.Workers[0].Reasons[0].Code != PlacementRegistration {
		t.Fatalf("nil entry: %+v", got)
	}
}

func TestPlacementResourceDetailAndFormatter(t *testing.T) {
	w := placementWorker()
	got := DiagnosePlacement([]*ateapi.Worker{w}, PlacementConstraints{Limits: &ateapi.Resources{CpuMillicores: 1500}})
	r := got.Workers[0].Reasons[0]
	if r.Code != PlacementCPU || r.Required != 1500 || r.Available != 1000 {
		t.Fatalf("resource detail: %+v", r)
	}
	text := got.String()
	if !strings.Contains(text, `worker "worker-a": insufficient CPU (required 1500, available 1000)`) || !strings.Contains(text, "native placement remains authoritative") {
		t.Fatal(text)
	}
	got = DiagnosePlacement([]*ateapi.Worker{w}, PlacementConstraints{})
	if !strings.Contains(got.String(), "no visible blocker") || strings.Contains(got.String(), "success") {
		t.Fatal(got.String())
	}
}
