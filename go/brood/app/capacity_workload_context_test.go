package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// FEATURE: Отмена admission до захвата project gate не блокирует shutdown.
// SPEC: Ждущий caller выходит с context error без Prepare/Apply; старый
// admission сохраняет поведение и разделяет тот же gate с новым API.
func contextAdmission(t *testing.T, s *Service) func(context.Context, string, string, WorkloadOperation) error {
	t.Helper()
	return s.AdmitWorkloadContext
}

func TestWorkloadAdmissionContextCancelsWhileProjectGateIsHeld(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, project, "one")
	admit := contextAdmission(t, e.svc)
	unlock := e.svc.lockProject(project)
	defer unlock()
	prepared := 0
	op := &capacityWorkload{prepare: func([]Swarm) (WorkloadAdmission, error) {
		prepared++
		return WorkloadAdmission{Name: "metadata", TemplateOnly: true}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- admit(ctx, idAlice, sw.ID, op) }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || prepared != 0 || op.applied != 0 {
			t.Fatalf("cancelled admission reached work: prepared=%d applied=%d %v", prepared, op.applied, err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled admission is still waiting behind another project operation")
	}
}

func TestWorkloadAdmissionContextRechecksCancellationAfterPreparation(t *testing.T) {
	e := newEnv(t)
	e.seedUsers(t)
	project := e.seedProject(t, idAlice)
	sw := mustCreateSwarm(t, e, idAlice, project, "one")
	admit := contextAdmission(t, e.svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	op := &capacityWorkload{prepare: func([]Swarm) (WorkloadAdmission, error) {
		cancel()
		return WorkloadAdmission{Name: "metadata", TemplateOnly: true}, nil
	}}
	if err := admit(ctx, idAlice, sw.ID, op); !errors.Is(err, context.Canceled) || op.applied != 0 {
		t.Fatalf("cancelled preparation reached mutation: applied=%d %v", op.applied, err)
	}
}
