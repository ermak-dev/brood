package app

import "testing"

// FEATURE: Non-initializing template publication does not reserve an actor (#48).
// SPEC: A trusted adapter marks only golden-disabled publication TemplateOnly;
// it still requires workload rights, a name and placement within the shard.
func TestTemplateOnlyAdmissionUsesNoActorReservation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector map[string]string
		replace  bool
		refuse   bool
	}{
		{name: "disabled golden template"},
		{name: "outside shard", selector: map[string]string{"shard": "two"}, refuse: true},
		{name: "not an actor replacement", replace: true, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.seedUsers(t)
			project := e.seedProject(t, idAlice)
			sw := mustCreateSwarm(t, e, idAlice, project, "one")
			e.svc.store = &capacityTestStore{Store: e.store, capacity: ShardCapacity{MaxActors: capacityLimit(0), MaxCPUMillicores: capacityLimit(0), MaxMemoryBytes: capacityLimit(0), WorkerSelector: map[string]string{"shard": "one"}}}
			op := &capacityWorkload{prepare: func(swarms []Swarm) (WorkloadAdmission, error) {
				return WorkloadAdmission{Name: "template", TemplateOnly: true, Replace: tc.replace, WorkerSelector: tc.selector}, nil
			}}
			err := e.svc.AdmitWorkload(idAlice, sw.ID, op)
			if tc.refuse {
				if err == nil || op.applied != 0 {
					t.Fatalf("invalid slotless operation accepted: %v", err)
				}
				return
			}
			if err != nil || op.applied != 1 || op.selector["shard"] != "one" {
				t.Fatalf("slotless publication refused: %v, calls=%d, selector=%v", err, op.applied, op.selector)
			}
		})
	}
}
