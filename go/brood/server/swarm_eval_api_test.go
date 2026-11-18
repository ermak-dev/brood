package server

// FEATURE: Двухэтапный eval API до загрузки акторов (#293).
// SPEC: Binding под общей project gate, пустой native inventory; start только
// из immutable selection, без git/provider эффектов; статус не даёт полномочий.
// IMPLEMENTATION: Metadata-only admission под project gate и локальная FULL-авторизация
// обоих scope; обычный start отказывает до разрешения HEAD.
// RESULT: IMPLEMENTED — focused normal/race, build, vet и полный Go suite проходят.
// STATUS: DONE

import (
	"context"
	"errors"

	"brood.dev/go/brood/app"
	"net/http"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/store"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

type evalAPIFixture struct {
	ts                                       *substrateServer
	alice                                    *http.Client
	candidate, reference, launcher, recorder string
	input                                    map[string]any
}

func evalAPISetup(t *testing.T) evalAPIFixture {
	t.Helper()
	ts := newSubstrateServer(t, newFakeAteapi())
	admin, alice, _, candidate := actorsSetup(t, ts.testServer)
	ctx := context.Background()
	s := ts.srv.store
	user, err := s.GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.GetUserByUsername(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	r, b, _ := ts.do(admin, "POST", "/api/projects", ts.csrf(admin), map[string]string{"name": "reference"})
	expectStatus(t, "reference project", r, b, http.StatusCreated)
	project := b["id"].(string)
	r, b, _ = ts.do(admin, "POST", "/api/projects/"+project+"/members", ts.csrf(admin), map[string]string{"username": "alice"})
	expectStatus(t, "reference membership", r, b, http.StatusNoContent)
	r, b, _ = ts.do(alice, "POST", "/api/projects/"+project+"/swarms", ts.csrf(alice), map[string]string{"name": "reference"})
	expectStatus(t, "reference swarm", r, b, http.StatusCreated)
	reference := b["id"].(string)
	for _, id := range []string{candidate, reference} {
		sw, e := s.GetSwarm(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		if e = s.SetProjectRepository(ctx, sw.ProjectID, startAPIOrigin); e != nil {
			t.Fatal(e)
		}
		if _, e = s.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"approve": startAPIApprove}, func(context.Context) bool { return true }); e != nil {
			t.Fatal(e)
		}
	}
	_, err = s.StartSwarm(ctx, store.SwarmStart{SwarmID: reference, UserID: user.ID, Definition: "approve", Goal: startAPIGoal, Commit: startAPICommit}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	requests, err := s.SwarmRequests(ctx, reference)
	if err != nil || len(requests) != 1 {
		t.Fatalf("reference requests: %v %v", requests, err)
	}
	_, err = s.AnswerSwarmRequest(ctx, store.SwarmAnswer{SwarmID: reference, RequestID: requests[0].ID, UserID: root.ID, ExpectedRevision: 1, Outcome: "success", Response: "Retained original notes"}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return evalAPIFixture{ts: ts, alice: alice, candidate: candidate, reference: reference, launcher: user.ID, recorder: root.ID, input: map[string]any{"referenceSwarmId": reference, "referencePass": 1, "definition": "approve", "model": "selected-model-1"}}
}
func (f evalAPIFixture) post(t *testing.T, suffix string, body any, want int) map[string]any {
	t.Helper()
	r, b, _ := f.ts.do(f.alice, "POST", "/api/swarms/"+f.candidate+suffix, f.ts.csrf(f.alice), body)
	expectStatus(t, suffix, r, b, want)
	return b
}
func TestEvalAPITwoStagePinnedFirstStart(t *testing.T) {
	f := evalAPISetup(t)
	s := f.ts.srv.store
	ctx := context.Background()
	f.post(t, "/eval", f.input, http.StatusOK)
	binding, err := s.GetSwarmEvalBinding(ctx, f.candidate)
	if err != nil || binding.Model != "selected-model-1" {
		t.Fatalf("binding: %+v %v", binding, err)
	}
	sw, _ := s.GetSwarm(ctx, f.candidate)
	root, _ := s.GetUserByUsername(ctx, "root")
	_, err = s.SyncWorkflowDefinitions(ctx, sw.ProjectID, root.ID, map[string]string{"approve": strings.ReplaceAll(startAPIApprove, "Approve?", "Changed?")}, func(context.Context) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	f.post(t, "/start", map[string]string{"definition": "approve", "goal": "other", "commit": startAPICommit}, http.StatusConflict)
	f.post(t, "/eval/start", nil, http.StatusOK)
	first, err := s.GetSwarmStep(ctx, f.candidate, 1)
	if err != nil || first.Goal != startAPIGoal || first.SourceCommit != startAPICommit {
		t.Fatalf("pin: %+v %v", first, err)
	}
	f.post(t, "/eval/start", nil, http.StatusConflict)
	f.post(t, "/eval", f.input, http.StatusOK)
	r, b, _ := f.ts.do(f.alice, "GET", "/api/swarms/"+f.candidate+"/eval", "", nil)
	expectStatus(t, "status", r, b, http.StatusOK)
}

// Обычный start не должен разрешать HEAD с installation credential.
func TestEvalAPIOrdinaryStartRefusedBeforeGit(t *testing.T) {
	f := evalAPISetup(t)
	f.post(t, "/eval", f.input, http.StatusOK)
	f.post(t, "/start", map[string]string{"definition": "approve", "goal": "other"}, http.StatusConflict)
}

func TestEvalAPIBindingRejectsNativeAndPendingGolden(t *testing.T) {
	for _, kind := range []string{"actor", "golden"} {
		t.Run(kind, func(t *testing.T) {
			f := evalAPISetup(t)
			sw, _ := f.ts.srv.store.GetSwarm(context.Background(), f.candidate)
			fake := f.ts.fake
			fake.mu.Lock()
			mode := ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED
			if kind == "golden" {
				mode = ateapi.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_ENABLED
			}
			fake.templates[sw.AtespaceName+"/native"] = inventoryTemplate(sw.AtespaceName, "native", "native-uid", mode)
			if kind == "actor" {
				fake.actors[sw.AtespaceName+"/actor"] = &ateapi.Actor{Metadata: fakeMeta("actor", sw.AtespaceName), ActorTemplate: &ateapi.ObjectRef{Atespace: sw.AtespaceName, Name: "native"}}
			}
			fake.mu.Unlock()
			f.post(t, "/eval", f.input, http.StatusConflict)
		})
	}
}
func TestEvalAPIReferenceManagementRequired(t *testing.T) {
	for _, stage := range []string{"bind", "retry", "start"} {
		t.Run(stage, func(t *testing.T) {
			f := evalAPISetup(t)
			s := f.ts.srv.store
			ctx := context.Background()
			if stage != "bind" {
				f.post(t, "/eval", f.input, http.StatusOK)
			}
			ref, err := s.GetSwarm(ctx, f.reference)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RemoveMember(ctx, ref.ProjectID, f.launcher); err != nil {
				t.Fatal(err)
			}
			if stage == "start" {
				f.post(t, "/eval/start", nil, http.StatusForbidden)
			} else {
				f.post(t, "/eval", f.input, http.StatusNotFound)
			}
			sw, _ := s.GetSwarm(ctx, f.candidate)
			if sw.Execution != "idle" || sw.Graph != nil {
				t.Fatalf("denied selection started: %+v", sw)
			}
			if stage == "bind" {
				if _, err = s.GetSwarmEvalBinding(ctx, f.candidate); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("denied binding persisted: %v", err)
				}
			}
		})
	}
}

func TestEvalAPIBindingRechecksLocalScopeAndSession(t *testing.T) {
	for _, change := range []string{"session", "namespace", "reference-membership"} {
		t.Run(change, func(t *testing.T) {
			f := evalAPISetup(t)
			ctx := context.Background()
			s := f.ts.srv.store
			live := true
			op := evalBindingOperation{server: f.ts.srv, ctx: ctx, input: store.SwarmEvalBindingInput{CandidateSwarmID: f.candidate, ReferenceSwarmID: f.reference, ReferencePass: 1, Definition: "approve", Model: "selected-model-1", LauncherID: f.launcher}, session: func(context.Context) bool { return live }}
			if _, err := op.Prepare(nil); err != nil {
				t.Fatal(err)
			}
			var err error
			switch change {
			case "session":
				live = false
			case "namespace":
				uid := "replaced"
				err = s.SetSwarmAtespaceUID(ctx, f.reference, &uid)
			case "reference-membership":
				ref, _ := s.GetSwarm(ctx, f.reference)
				err = s.RemoveMember(ctx, ref.ProjectID, f.launcher)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = op.Apply(nil); err == nil {
				t.Fatal("stale authority committed binding")
			}
			if _, err = s.GetSwarmEvalBinding(ctx, f.candidate); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("denied marker: %v", err)
			}
		})
	}
}

func TestEvalAPIOwnerInterruptionIsSeparate(t *testing.T) {
	f := evalAPISetup(t)
	ctx := context.Background()
	s := f.ts.srv.store
	f.post(t, "/eval", f.input, http.StatusOK)
	b, _ := s.GetSwarmEvalBinding(ctx, f.candidate)
	scopes, err := f.ts.srv.evalScopes(ctx, f.candidate, f.reference)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.GetUserByUsername(ctx, "root")
	if err = s.SetSwarmMember(ctx, f.candidate, root.ID, app.RoleOwner); err != nil {
		t.Fatal(err)
	}
	if err = s.SetSwarmMember(ctx, f.candidate, f.launcher, app.RoleEditor); err != nil {
		t.Fatal(err)
	}
	err = s.WithDurableTx(ctx, func(tx context.Context) error {
		if err := f.ts.srv.evalAuthority(scopes, nil, false)(tx, "replay_eval", b); err != nil {
			return err
		}
		if err := f.ts.srv.evalAuthority(scopes, nil, true)(tx, "interrupt_eval_children", b); err == nil {
			t.Fatal("editor received owner child-interruption authority")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEvalAPIBindingUsesSharedProjectGate(t *testing.T) {
	f := evalAPISetup(t)
	hold := ephemeralGateHold{entered: make(chan struct{}), release: make(chan struct{})}
	held := make(chan error, 1)
	go func() { held <- f.ts.srv.app.AdmitWorkload(f.launcher, f.candidate, hold) }()
	<-hold.entered
	done := make(chan int, 1)
	go func() {
		r, _, _ := f.ts.do(f.alice, "POST", "/api/swarms/"+f.candidate+"/eval", f.ts.csrf(f.alice), f.input)
		done <- r.StatusCode
	}()
	early := false
	select {
	case <-done:
		early = true
	case <-time.After(100 * time.Millisecond):
	}
	close(hold.release)
	<-held
	if early {
		t.Fatal("eval binding escaped shared project gate")
	}
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("binding status %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("binding did not resume")
	}
}
