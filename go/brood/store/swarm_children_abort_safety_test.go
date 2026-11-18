package store

// BUG: Recovery неполной группы не должно зависеть от живого worker grant
// или отменять уже начавшийся/неопределённый эффект.
// HYPOTHESIS: Отдельная terminal запись с SQL fences и BEGIN IMMEDIATE
// позволяет безопасный abort лишь pending/cancelled истории, без обхода барьеров.
// SOLUTION: Независимая текущая web session owner/admin, policy и повторная
// проверка личности в той же транзакции; abort/start сериализованы SQLite.
// RESULT: FIXED — reauthorization, CAS, rollback и SQL fences сохраняют
// running/uncertain эффекты; точные terminal повторы не меняют историю.
// STATUS: FIXED

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func childRecoverySession(t *testing.T, s *Store, human string) string {
	t.Helper()
	session := "recover-" + human
	mustNil(t, s.CreateSession(context.Background(), Session{TokenHash: session, UserID: human}))
	return session
}

func reopenChildStore(t *testing.T, s *Store) *Store {
	t.Helper()
	var path, dbname string
	var seq int
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbname, &path))
	mustNil(t, s.Close())
	reopened, err := Open(context.Background(), path)
	mustNil(t, err)
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

func TestSwarmChildAbortPartialRestartAndRevokedWorkers(t *testing.T) {
	ctx := context.Background()
	for _, membership := range []string{"empty", "pending", "mixed"} {
		t.Run(membership, func(t *testing.T) {
			s, p, e := childFixture(t, "quorum", 3, 2)
			session := childRecoverySession(t, s, "u")
			var before []SwarmChildTask
			if membership != "empty" {
				before = append(before, reserveChild(t, s, p, e, "pending"))
			}
			if membership == "mixed" {
				c := reserveChild(t, s, p, e, "cancelled")
				c, err := s.CancelSwarmChild(ctx, p, c.RequestID, c.Revision, childAllow)
				mustNil(t, err)
				before = append(before, c)
			}
			// Оба worker principal уже устарели; human recovery не делегирует их права.
			setActor(t, s, "a", "incarnation=2")
			setActor(t, s, "b", "incarnation=2")
			s = reopenChildStore(t, s)
			var open int
			mustNil(t, s.db.QueryRow(`SELECT count(*) FROM swarm_child_joins WHERE state='open'`).Scan(&open))
			if open != 1 {
				t.Fatal("restart lost partial join")
			}
			checked := false
			abort, err := s.AbortSwarmChildJoin(ctx, p, session, 1, func(txctx context.Context, a SwarmChildAuthorization) error {
				if _, ok := txctx.Value(txKey{}).(*sql.Tx); !ok {
					t.Fatal("recovery policy outside transaction")
				}
				if a.Action != "abort" || a.HumanID != "u" || a.SessionHash != session || a.Parent != p || a.Caller != (SwarmChildPrincipal{}) {
					t.Fatalf("recovery authority context: %+v", a)
				}
				checked = true
				return nil
			})
			mustNil(t, err)
			if !checked || abort.State != "aborted" || abort.Revision != 2 || abort.AbortedBy != "u" || abort.AbortedAt == "" || abort.Policy.Total != 3 || abort.Policy.Quorum != 2 || abort.Evaluation.Open != 0 || len(abort.Children) != len(before) {
				t.Fatalf("partial abort: %+v", abort)
			}
			for i, c := range before {
				got := abort.Children[i]
				if c.State == "pending" {
					c.State = "cancelled"
					c.Revision++
					c.FinishedAt = abort.AbortedAt
				}
				if !reflect.DeepEqual(c, got) {
					t.Fatalf("changed immutable child history: %+v -> %+v", c, got)
				}
			}
			s = reopenChildStore(t, s)
			retry, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
			mustNil(t, err)
			if !reflect.DeepEqual(abort, retry) {
				t.Fatal("restart/retry changed abort")
			}
			_, err = s.StopSwarm(ctx, "one", "u", allow)
			mustNil(t, err)
		})
	}
}

func TestSwarmChildAbortRefusesStartedOrFullGroups(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"running", "uncertain", "completed", "full pending", "full cancelled"} {
		t.Run(state, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 3, 0)
			session := childRecoverySession(t, s, "u")
			reserveChild(t, s, p, e, "pending")
			c := reserveChild(t, s, p, e, "other")
			switch state {
			case "running", "uncertain", "completed":
				var err error
				c, err = s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
				mustNil(t, err)
				if state == "completed" {
					_, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "fail", "result", childAllow)
					mustNil(t, err)
				}
				if state == "uncertain" {
					setActor(t, s, "b", "incarnation=2")
					_, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "unverified", childAllow)
					if !errors.Is(err, ErrSwarmChildDenied) {
						t.Fatalf("uncertain completion: %v", err)
					}
					s = reopenChildStore(t, s)
				}
			default:
				reserveChild(t, s, p, e, "third")
				if state == "full cancelled" {
					join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
					mustNil(t, err)
					for _, c := range join.Children {
						_, err = s.CancelSwarmChild(ctx, p, c.RequestID, c.Revision, childAllow)
						mustNil(t, err)
					}
				}
			}
			before := dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks", "swarm_steps", "swarms")
			_, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
			if !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("abort %s: %v", state, err)
			}
			if dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks", "swarm_steps", "swarms") != before {
				t.Fatal("rejected abort changed obligations")
			}
			if state != "full cancelled" {
				if err := s.CheckActorChildObligations(ctx, p.Principal.ActorUID); !errors.Is(err, ErrCoordinationConflict) {
					t.Fatalf("rejected abort released automatic child hold: %v", err)
				}
				if err := s.CompletePipelineActorVisit(ctx, "one", "one", 1, "parent", "a", 1, "success", ""); err == nil {
					t.Fatal("rejected abort released parent")
				}
			}
		})
	}
}

func TestSwarmChildAbortCurrentHumanAuthority(t *testing.T) {
	ctx := context.Background()
	for _, drift := range []string{"nil policy", "denied policy", "missing session", "expired session", "telegram session", "disabled", "nonmember", "editor", "viewer", "foreign owner", "callback logout", "callback disabled", "callback transfer", "callback nonmember", "callback identity", "callback expiry"} {
		t.Run(drift, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 2, 0)
			reserveChild(t, s, p, e, "pending")
			session := childRecoverySession(t, s, "u")
			policy := SwarmChildAuthorize(childAllow)
			mutate := func(ctx context.Context, statement string) error {
				_, err := s.conn(ctx).ExecContext(ctx, statement)
				return err
			}
			switch drift {
			case "nil policy":
				policy = nil
			case "denied policy":
				policy = func(context.Context, SwarmChildAuthorization) error { return ErrSwarmChildDenied }
			case "missing session":
				session = "absent"
			case "expired session":
				mustNil(t, mutate(ctx, `UPDATE sessions SET expires_at='2000-01-01T00:00:00Z'`))
			case "telegram session":
				mustNil(t, mutate(ctx, `UPDATE sessions SET kind='telegram'`))
			case "disabled":
				mustNil(t, mutate(ctx, `UPDATE users SET disabled=1 WHERE id='u'`))
			case "nonmember":
				mustNil(t, mutate(ctx, `DELETE FROM project_members WHERE project_id='one' AND user_id='u'`))
			case "editor", "viewer":
				_, err := s.db.Exec(`UPDATE swarm_members SET role=? WHERE swarm_id='one' AND user_id='u'`, drift)
				mustNil(t, err)
			case "foreign owner":
				mustNil(t, s.CreateUser(ctx, User{ID: "foreign", Username: "foreign", PasswordHash: "hash"}))
				mustNil(t, s.AddMember(ctx, "two", "foreign"))
				mustNil(t, s.SetSwarmOwner(ctx, "two", "foreign"))
				session = childRecoverySession(t, s, "foreign")
			case "callback logout":
				policy = func(ctx context.Context, _ SwarmChildAuthorization) error { return s.DeleteSession(ctx, session) }
			case "callback disabled":
				policy = func(ctx context.Context, _ SwarmChildAuthorization) error {
					return mutate(ctx, `UPDATE users SET disabled=1 WHERE id='u'`)
				}
			case "callback transfer":
				policy = func(ctx context.Context, _ SwarmChildAuthorization) error {
					return mutate(ctx, `UPDATE swarm_members SET role='editor' WHERE swarm_id='one' AND user_id='u'`)
				}
			case "callback nonmember":
				policy = func(ctx context.Context, _ SwarmChildAuthorization) error {
					return mutate(ctx, `DELETE FROM project_members WHERE project_id='one' AND user_id='u'`)
				}
			case "callback identity":
				mustNil(t, s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "hash", IsAdmin: true}))
				policy = func(ctx context.Context, _ SwarmChildAuthorization) error {
					return mutate(ctx, `UPDATE sessions SET user_id='admin' WHERE token_hash='recover-u'`)
				}
			case "callback expiry":
				policy = func(ctx context.Context, _ SwarmChildAuthorization) error {
					return mutate(ctx, `UPDATE sessions SET expires_at='2000-01-01T00:00:00Z'`)
				}
			}
			before := dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks", "sessions", "users", "project_members", "swarm_members")
			_, err := s.AbortSwarmChildJoin(ctx, p, session, 1, policy)
			if !errors.Is(err, ErrSwarmChildDenied) {
				t.Fatalf("%s authority: %v", drift, err)
			}
			if dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks", "sessions", "users", "project_members", "swarm_members") != before {
				t.Fatal("denial did not roll back atomically")
			}
		})
	}
}

func TestSwarmChildAbortAdminAndRetryFences(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 2, 0)
	c := reserveChild(t, s, p, e, "pending")
	mustNil(t, s.CreateUser(ctx, User{ID: "admin", Username: "admin", PasswordHash: "hash", IsAdmin: true}))
	session := childRecoverySession(t, s, "admin")
	for _, expected := range []int64{0, 2, 999} {
		if _, err := s.AbortSwarmChildJoin(ctx, p, session, expected, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
			t.Fatalf("revision %d: %v", expected, err)
		}
	}
	wrong := p
	wrong.RequestID = "not-parent"
	if _, err := s.AbortSwarmChildJoin(ctx, wrong, session, 1, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("wrong parent: %v", err)
	}
	before := dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks")
	rollback := errors.New("rollback abort")
	err := s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
		if err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) || dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks") != before {
		t.Fatalf("abort escaped rollback: %v", err)
	}
	abort, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
	mustNil(t, err)
	if abort.AbortedBy != "admin" {
		t.Fatal("admin recovery was not journaled")
	}
	// Терминальная группа не допускает ни новой работы, ни переписывания abort.
	before = dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks")
	_, err = s.ReserveSwarmChild(ctx, p, "new", c.Payload, e, childAllow)
	if !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("new aborted reservation: %v", err)
	}
	_, err = s.ReserveSwarmChild(ctx, p, c.RequestID, "changed", e, childAllow)
	if !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("changed aborted reservation: %v", err)
	}
	reserved, err := s.ReserveSwarmChild(ctx, p, c.RequestID, c.Payload, e, childAllow)
	mustNil(t, err)
	cancelled, err := s.CancelSwarmChild(ctx, p, c.RequestID, c.Revision, childAllow)
	mustNil(t, err)
	if !reflect.DeepEqual(reserved, abort.Children[0]) || !reflect.DeepEqual(cancelled, abort.Children[0]) {
		t.Fatal("exact child retries after abort changed cancelled history")
	}
	if _, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("aborted child start: %v", err)
	}
	if _, err := s.AbortSwarmChildJoin(ctx, p, session, 2, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("changed retry revision: %v", err)
	}
	owner := childRecoverySession(t, s, "u")
	if _, err := s.AbortSwarmChildJoin(ctx, p, owner, 1, childAllow); !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("changed abort human: %v", err)
	}
	retry, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
	mustNil(t, err)
	if !reflect.DeepEqual(abort, retry) {
		t.Fatal("exact admin retry changed history")
	}
	if dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks") != before {
		t.Fatal("terminal retries changed history")
	}
	mustNil(t, s.DeleteSession(ctx, session))
	if _, err := s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow); !errors.Is(err, ErrSwarmChildDenied) {
		t.Fatalf("revoked exact retry: %v", err)
	}
}

func TestSwarmChildAbortStartIndependentConnectionRace(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 2, 0)
	c := reserveChild(t, s, p, e, "race")
	session := childRecoverySession(t, s, "u")
	var file, name string
	var seq int
	mustNil(t, s.db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file))
	second, err := Open(ctx, file)
	mustNil(t, err)
	defer second.Close()
	start := make(chan struct{})
	var abortErr, startErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, abortErr = s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow) }()
	go func() {
		defer wg.Done()
		<-start
		_, startErr = second.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
	}()
	close(start)
	wg.Wait()
	successes := 0
	for _, err := range []error{abortErr, startErr} {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrSwarmChildConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("abort/start accepted %d claims; abort=%v start=%v", successes, abortErr, startErr)
	}
	join, err := s.GetSwarmChildJoin(ctx, p, childAllow)
	mustNil(t, err)
	if abortErr == nil {
		if join.State != "aborted" || join.Children[0].State != "cancelled" || join.Evaluation.Open != 0 {
			t.Fatalf("abort winner: %+v", join)
		}
	} else {
		if join.State != "open" || join.Children[0].State != "running" || join.Evaluation.Open != 2 {
			t.Fatalf("start winner: %+v", join)
		}
		_, err := s.StopSwarm(ctx, "one", "u", allow)
		mustNil(t, err)
		afterStop, err := s.GetSwarmChildJoin(ctx, p, childAllow)
		mustNil(t, err)
		if !afterStop.Interrupted || afterStop.State != join.State || afterStop.Policy != join.Policy || afterStop.Evaluation.Open != join.Evaluation.Open || !reflect.DeepEqual(afterStop.Children, join.Children) {
			t.Fatalf("owner stop lost start winner obligations: before=%+v after=%+v", join, afterStop)
		}
		if err := s.CheckActorChildObligations(ctx, p.Principal.ActorUID); !errors.Is(err, ErrCoordinationConflict) {
			t.Fatalf("owner stop released start winner automatic hold: %v", err)
		}
	}
}

func TestSwarmChildAbortRawSQLFences(t *testing.T) {
	ctx := context.Background()
	for _, recursive := range []string{"0", "1"} {
		t.Run(recursive, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 2, 0)
			c := reserveChild(t, s, p, e, "child")
			session := childRecoverySession(t, s, "u")
			_, err := s.db.Exec(`PRAGMA recursive_triggers=` + recursive)
			mustNil(t, err)
			before := dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks")
			// Pending нельзя спрятать в aborted даже прямым SQL.
			_, err = s.db.Exec(`UPDATE swarm_child_joins SET state='aborted',revision=2,aborted_by='u',aborted_at='now'`)
			if err == nil {
				t.Fatal("raw abort concealed pending obligations")
			}
			if dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks") != before {
				t.Fatal("raw refusal changed history")
			}
			_, err = s.AbortSwarmChildJoin(ctx, p, session, 1, childAllow)
			mustNil(t, err)
			before = dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks")
			for _, statement := range []string{
				`UPDATE swarm_child_joins SET state='open',revision=1,aborted_by=NULL,aborted_at=NULL`,
				`UPDATE swarm_child_joins SET aborted_by='other'`,
				`UPDATE swarm_child_joins SET aborted_at='later'`,
				`UPDATE swarm_child_joins SET total=1`,
				`UPDATE swarm_child_joins SET parent_request_id='other'`,
				`UPDATE swarm_child_joins SET created_at='later'`,
				`DELETE FROM swarm_child_joins`,
				`INSERT OR REPLACE INTO swarm_child_joins SELECT * FROM swarm_child_joins`,
				`DELETE FROM swarm_child_tasks`,
				`UPDATE swarm_child_tasks SET state='running',finished_at=NULL,revision=revision+1`,
				`INSERT INTO swarm_child_tasks(id,swarm_id,parent_sequence,request_id,payload,actor_uid,actor_incarnation,state,revision,created_at) VALUES('new','one',1,'new','task','b',1,'pending',1,'now')`,
			} {
				if _, err := s.db.Exec(statement); err == nil {
					t.Errorf("aborted history fence accepted %s", statement)
				}
			}
			if dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks") != before {
				t.Fatal("aborted history mutated")
			}
			if _, err := s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "success", "late", childAllow); !errors.Is(err, ErrSwarmChildConflict) {
				t.Fatalf("late result after abort: %v", err)
			}
		})
	}
}

func TestSwarmChildAbortRechecksGroupAfterPolicy(t *testing.T) {
	ctx := context.Background()
	s, p, e := childFixture(t, "all", 2, 0)
	c := reserveChild(t, s, p, e, "pending")
	session := childRecoverySession(t, s, "u")
	before := dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks")
	_, err := s.AbortSwarmChildJoin(ctx, p, session, 1, func(ctx context.Context, _ SwarmChildAuthorization) error {
		// Adversarial callback не делает внешних эффектов, но меняет intent.
		_, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
		return err
	})
	if !errors.Is(err, ErrSwarmChildConflict) {
		t.Fatalf("abort used pre-policy snapshot: %v", err)
	}
	if dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks") != before {
		t.Fatal("stale snapshot abort did not roll back")
	}
}

func TestSwarmChildAbortRefusesRawRunningOrCompleted(t *testing.T) {
	ctx := context.Background()
	for _, state := range []string{"running", "completed"} {
		t.Run(state, func(t *testing.T) {
			s, p, e := childFixture(t, "all", 2, 0)
			c := reserveChild(t, s, p, e, "started")
			c, err := s.StartSwarmChild(ctx, p, c.RequestID, e, c.Revision, childAllow)
			mustNil(t, err)
			if state == "completed" {
				_, err = s.CompleteSwarmChild(ctx, p, c.RequestID, e, c.Revision, "fail", "result", childAllow)
				mustNil(t, err)
			}
			before := dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks")
			_, err = s.db.Exec(`UPDATE swarm_child_joins SET state='aborted',revision=2,aborted_by='u',aborted_at='now'`)
			if err == nil {
				t.Fatalf("raw abort accepted %s effects", state)
			}
			if dumpTables(t, s, "swarm_child_joins", "swarm_child_tasks") != before {
				t.Fatal("raw abort lost started history")
			}
		})
	}
}
