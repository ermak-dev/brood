package app

import (
	"errors"
	"maps"
	"testing"
	"testing/synctest"
)

// FEATURE: Authorize and serialize workload admission against shard reservations (#48).
// SPEC: Replacement does not double-count, selectors remain in the shard,
// failed inventory or revoked access never reaches the native mutation.
type capacityWorkload struct {
	prepare    func([]Swarm) (WorkloadAdmission, error)
	applied    int
	selector   map[string]string
	applyError error
	onApply    func()
}

func (w *capacityWorkload) Prepare(swarms []Swarm) (WorkloadAdmission, error) {
	return w.prepare(swarms)
}
func (w *capacityWorkload) Apply(selector map[string]string) error {
	w.applied++
	if w.onApply != nil {
		w.onApply()
	}
	w.selector = maps.Clone(selector)
	return w.applyError
}

func TestWorkloadAdmissionSerializesInventoryThroughApply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.seedUsers(t)
		project := e.seedProject(t, idAlice)
		sw := mustCreateSwarm(t, e, idAlice, project, "one")
		e.svc.store = &capacityTestStore{Store: e.store, capacity: ShardCapacity{MaxActors: capacityLimit(1)}}
		var reservations []ActorReservation
		prepared := 0
		entered, release := make(chan struct{}), make(chan struct{})
		makeOperation := func(name string) *capacityWorkload {
			return &capacityWorkload{
				prepare: func(swarms []Swarm) (WorkloadAdmission, error) {
					prepared++
					return WorkloadAdmission{Reservations: append([]ActorReservation(nil), reservations...), Name: name, CPUMillicores: 1000, MemoryBytes: 1024}, nil
				},
				onApply: func() {
					if name == "first" {
						close(entered)
						<-release
					}
					reservations = append(reservations, ActorReservation{SwarmID: sw.ID, Name: name, CPUMillicores: 1000, MemoryBytes: 1024})
				},
			}
		}
		results := make(chan error, 2)
		first, second := makeOperation("first"), makeOperation("second")
		go func() { results <- e.svc.AdmitWorkload(idAlice, sw.ID, first) }()
		<-entered
		go func() { results <- e.svc.AdmitWorkload(idAlice, sw.ID, second) }()
		synctest.Wait()
		if prepared != 1 {
			t.Fatal("second operation inspected stale inventory during first native mutation")
		}
		close(release)
		synctest.Wait()
		a, b := <-results, <-results
		if a != nil && b == nil {
			a, b = b, a
		}
		var exceeded *CapacityExceededError
		if a != nil || !errors.As(b, &exceeded) || first.applied != 1 || second.applied != 0 {
			t.Fatalf("last slot admitted incorrectly: %v, %v; calls=%d/%d", a, b, first.applied, second.applied)
		}
	})
}

func TestWorkloadAdmissionChecksResourcesAndReplacement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replace     bool
		cpu, memory int64
		reject      bool
	}{
		{"create fits", false, 1000, 1024, false},
		{"create cpu exhausted", false, 1001, 1024, true},
		{"create memory exhausted", false, 1000, 1025, true},
		{"replacement same size", true, 1000, 1024, false},
		{"replacement grows", true, 2001, 1024, true},
		{"missing resolved resources", false, 0, 1024, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			sw := mustCreateSwarm(t, e, idAlice, project, "one")
			e.svc.store = &capacityTestStore{Store: e.store, capacity: ShardCapacity{WorkerSelector: map[string]string{"shard": "one"}, MaxActors: capacityLimit(2), MaxCPUMillicores: capacityLimit(2000), MaxMemoryBytes: capacityLimit(2048)}}
			op := &capacityWorkload{prepare: func(swarms []Swarm) (WorkloadAdmission, error) {
				return WorkloadAdmission{Reservations: []ActorReservation{{SwarmID: sw.ID, Name: "existing", CPUMillicores: 1000, MemoryBytes: 1024}}, Name: map[bool]string{false: "new", true: "existing"}[tc.replace], Replace: tc.replace, CPUMillicores: tc.cpu, MemoryBytes: tc.memory}, nil
			}}
			err := e.svc.AdmitWorkload(idAlice, sw.ID, op)
			if tc.reject {
				if err == nil || op.applied != 0 {
					t.Fatalf("invalid admission applied: %v, calls=%d", err, op.applied)
				}
				return
			}
			if err != nil || op.applied != 1 || op.selector["shard"] != "one" {
				t.Fatalf("admission=%v, calls=%d, selector=%v", err, op.applied, op.selector)
			}
		})
	}
}

func TestWorkloadAdmissionCanDrainAfterLimitsAreLowered(t *testing.T) {
	for _, tc := range []struct {
		name    string
		replace bool
		cpu     int64
		refuse  bool
	}{
		{"shrink above new limit", true, 2000, false},
		{"unchanged reservation", true, 3000, false},
		{"grow above new limit", true, 3001, true},
		{"additional actor", false, 100, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			sw := mustCreateSwarm(t, e, idAlice, project, "one")
			e.svc.store = &capacityTestStore{Store: e.store, capacity: ShardCapacity{MaxSwarms: capacityLimit(0), MaxActors: capacityLimit(1), MaxCPUMillicores: capacityLimit(2000)}}
			op := &capacityWorkload{prepare: func(swarms []Swarm) (WorkloadAdmission, error) {
				return WorkloadAdmission{
					Reservations: []ActorReservation{{Name: "old", SwarmID: sw.ID, CPUMillicores: 3000, MemoryBytes: 1024}, {Name: "other", SwarmID: sw.ID, CPUMillicores: 1000, MemoryBytes: 1024}},
					Name:         map[bool]string{true: "old", false: "new"}[tc.replace], Replace: tc.replace, CPUMillicores: tc.cpu, MemoryBytes: 1024,
				}, nil
			}}
			err := e.svc.AdmitWorkload(idAlice, sw.ID, op)
			if tc.refuse {
				if err == nil || op.applied != 0 {
					t.Fatalf("growth accepted after limit lowered: %v", err)
				}
			} else if err != nil || op.applied != 1 {
				t.Fatalf("existing reservation cannot drain: %v", err)
			}
		})
	}
}

func TestWorkloadAdmissionFailsClosed(t *testing.T) {
	for _, scenario := range []string{"inventory error", "revoked during read", "foreign selector", "replace missing", "duplicate create", "apply error"} {
		t.Run(scenario, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			sw := mustCreateSwarm(t, e, idAlice, project, "one")
			e.svc.store = &capacityTestStore{Store: e.store, capacity: ShardCapacity{WorkerSelector: map[string]string{"shard": "one"}}}
			sentinel := errors.New("upstream failed")
			op := &capacityWorkload{prepare: func(swarms []Swarm) (WorkloadAdmission, error) {
				plan := WorkloadAdmission{Name: "new", CPUMillicores: 1000, MemoryBytes: 1024}
				switch scenario {
				case "inventory error":
					return plan, sentinel
				case "revoked during read":
					if err := e.store.RemoveMembership(project, idAlice); err != nil {
						t.Fatal(err)
					}
				case "foreign selector":
					plan.WorkerSelector = map[string]string{"shard": "two"}
				case "replace missing":
					plan.Replace = true
				case "duplicate create":
					plan.Reservations = []ActorReservation{{SwarmID: sw.ID, Name: "new", CPUMillicores: 1000, MemoryBytes: 1024}}
				}
				return plan, nil
			}}
			if scenario == "apply error" {
				op.applyError = sentinel
			}
			err := e.svc.AdmitWorkload(idAlice, sw.ID, op)
			if err == nil {
				t.Fatal("failed operation accepted")
			}
			if scenario == "apply error" {
				if !errors.Is(err, sentinel) || op.applied != 1 {
					t.Fatalf("apply failure lost: %v, calls=%d", err, op.applied)
				}
			} else if op.applied != 0 {
				t.Fatal("refused operation reached native mutation")
			}
		})
	}
}
