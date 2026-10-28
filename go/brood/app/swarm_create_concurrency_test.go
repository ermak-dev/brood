package app

import (
	"testing"
	"testing/synctest"
)

// FEATURE: Concurrent retries must not classify an in-flight own atespace as foreign.
// SPEC: The first RPC commits remotely before its UID is saved locally; another
// create for the same project/name waits and returns the same active swarm.
// IMPLEMENTATION: Per-project gates serialize creation and repeat authorization.
// RESULT: IMPLEMENTED — 100 deterministic synctest repetitions and the full race suite pass.
func TestInFlightSwarmCreateRetriesAreSerialized(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := newEnv(t)
		e.seedUsers(t)
		project := e.seedProject(t, idAlice)
		paused := &pausedCreateProvisioner{
			fakeProvisioner: e.prov, created: make(chan struct{}), release: make(chan struct{}),
		}
		e.svc.provisioner = paused
		type outcome struct {
			swarm Swarm
			err   error
		}
		results := make(chan outcome, 2)
		create := func() {
			swarm, err := e.svc.CreateSwarm(idAlice, project, "alpha")
			results <- outcome{swarm, err}
		}
		go create()
		<-paused.created
		go create()
		synctest.Wait()
		observed := []outcome{}
		select {
		case early := <-results:
			observed = append(observed, early)
			t.Errorf("retry returned before the original create completed: %v", early.err)
		default:
		}
		close(paused.release)
		for len(observed) < 2 {
			observed = append(observed, <-results)
		}
		for _, result := range observed {
			if result.err != nil || result.swarm.State != StateActive || result.swarm.ID != "s1" || result.swarm.AtespaceUID != "uid-1" {
				t.Errorf("concurrent create result = %+v, %v", result.swarm, result.err)
			}
		}
	})
}

type pausedCreateProvisioner struct {
	*fakeProvisioner
	created chan struct{}
	release chan struct{}
}

func (p *pausedCreateProvisioner) CreateAtespace(name string) (string, error) {
	uid, err := p.fakeProvisioner.CreateAtespace(name)
	if err != nil {
		return "", err
	}
	close(p.created)
	<-p.release
	return uid, nil
}
