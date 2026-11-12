package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

// FEATURE: Строка списка роёв несёт исполнение и открытые запросы, строка
// проекта — число роёв (#234).
// SPEC: сводка набора роёв и счёт роёв набора проектов читаются одной
// выборкой каждая, без запроса на рой или проект; открытые запросы считаются
// по всем запросам роя, узловым и актора; счёт роёв следует правилу списка
// роёв проекта: администратору все неудалённые, участнику — с его ролью.
// IMPLEMENTATION: SwarmSummaries и ProjectSwarmCounts, swarm_summary.go.
// RESULT: IMPLEMENTED
// STATUS: DONE

// countingDB считает обращения к базе: сводка обязана уложиться в одно.
type countingDB struct {
	DB
	calls int
}

func (c *countingDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.calls++
	return c.DB.ExecContext(ctx, query, args...)
}

func (c *countingDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.calls++
	return c.DB.QueryContext(ctx, query, args...)
}

func (c *countingDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	c.calls++
	return c.DB.QueryRowContext(ctx, query, args...)
}

// summaryStore: рой s ждёт оператора на узле и двух вопросах актора (ещё
// один отвечен, один отменён), busy идёт с одним вопросом актора, quiet не
// запускался и вопросов не имеет.
func summaryStore(t *testing.T) *Store {
	t.Helper()
	s := swarmTestStore(t, "busy", "quiet")
	if _, err := s.db.Exec(`UPDATE swarms SET execution='waiting' WHERE id='s';
		UPDATE swarms SET execution='running' WHERE id='busy';
		INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('s',1,'ask','waiting');
		INSERT INTO swarm_requests(id,swarm_id,sequence,origin,kind,question,options,state,created_at) VALUES ('n1','s',1,'node','choice','Ship?',x'5b5d','open','now');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at) VALUES ('a1','s','actor','w',1,'text','q','open','now');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at) VALUES ('a2','s','actor','w',1,'confirm','q','open','now');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at,answered_at,answered_by,response,delivery) VALUES ('a3','s','actor','w',1,'text','q','answered','now','now','u','done','delivered');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at) VALUES ('a4','s','actor','w',1,'text','q','cancelled','now');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at) VALUES ('b1','busy','actor','w',1,'text','q','open','now')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSwarmSummariesCountOpenRequestsOfEveryOrigin(t *testing.T) {
	s := summaryStore(t)
	got, err := s.SwarmSummaries(context.Background(), []string{"s", "busy", "quiet", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]SwarmSummary{
		"s":     {Execution: "waiting", OpenRequests: 3, MemberCount: 1},
		"busy":  {Execution: "running", OpenRequests: 1},
		"quiet": {Execution: "idle", OpenRequests: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summaries:\n got %+v\nwant %+v", got, want)
	}
	// Рой вне набора не читается, даже если он есть в базе.
	got, err = s.SwarmSummaries(context.Background(), []string{"busy"})
	if err != nil || !reflect.DeepEqual(got, map[string]SwarmSummary{"busy": {Execution: "running", OpenRequests: 1}}) {
		t.Fatalf("one swarm: %+v %v", got, err)
	}
}

// FEATURE: Строка списка роёв несёт число недоставленных ответов актору (#266).
// SPEC: недоставленный ответ — отвеченный запрос актора с доставкой failed
// или unconfirmed; доставленный и ещё идущий (pending) ответ, ответ на
// узловой запрос, открытый и отменённый запрос не считаются; счёт — по
// своему рою, той же выборкой, что исполнение и открытые запросы.
// IMPLEMENTATION: SwarmSummary.UndeliveredAnswers, swarmSummaries.
// RESULT: IMPLEMENTED
// STATUS: DONE

// undeliveredStore: у роя s по ответу актору в каждом состоянии доставки
// (unconfirmed дважды), отвеченный узловой запрос, открытый и отменённый
// вопросы актора; у calm ответы только доставленные и идущие; у busy один
// failed; quiet запросов не имеет.
func undeliveredStore(t *testing.T) *Store {
	t.Helper()
	s := swarmTestStore(t, "calm", "busy", "quiet")
	if _, err := s.db.Exec(`UPDATE swarms SET execution='running' WHERE id IN ('s','busy');
		INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES ('s',1,'ask','answered');
		INSERT INTO swarm_requests(id,swarm_id,sequence,origin,kind,question,options,state,created_at,answered_at,answered_by,outcome) VALUES ('n1','s',1,'node','choice','Ship?',x'5b5d','answered','now','now','u','success');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at) VALUES ('open','s','actor','w',1,'text','q','open','now');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at) VALUES ('gone','s','actor','w',1,'text','q','cancelled','now');
		INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at,answered_at,answered_by,response,delivery) VALUES
			('s-delivered','s','actor','w',1,'text','q','answered','now','now','u','r','delivered'),
			('s-pending','s','actor','w',1,'text','q','answered','now','now','u','r','pending'),
			('s-failed','s','actor','w',1,'text','q','answered','now','now','u','r','failed'),
			('s-unconfirmed-1','s','actor','w',1,'text','q','answered','now','now','u','r','unconfirmed'),
			('s-unconfirmed-2','s','actor','w',2,'confirm','q','answered','now','now','u','r','unconfirmed'),
			('c-delivered','calm','actor','w',1,'text','q','answered','now','now','u','r','delivered'),
			('c-pending','calm','actor','w',1,'text','q','answered','now','now','u','r','pending'),
			('b-failed','busy','actor','w',1,'text','q','answered','now','now','u','r','failed')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSwarmSummariesCountUndeliveredActorAnswers(t *testing.T) {
	s := undeliveredStore(t)
	got, err := s.SwarmSummaries(context.Background(), []string{"s", "calm", "busy", "quiet"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]SwarmSummary{
		"s":     {Execution: "running", OpenRequests: 1, UndeliveredAnswers: 3, MemberCount: 1},
		"calm":  {Execution: "idle", OpenRequests: 0, UndeliveredAnswers: 0},
		"busy":  {Execution: "running", OpenRequests: 0, UndeliveredAnswers: 1},
		"quiet": {Execution: "idle", OpenRequests: 0, UndeliveredAnswers: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summaries:\n got %+v\nwant %+v", got, want)
	}
	// Ответы чужого роя в счёт не входят, и рой вне набора не читается.
	got, err = s.SwarmSummaries(context.Background(), []string{"calm"})
	if err != nil || !reflect.DeepEqual(got, map[string]SwarmSummary{"calm": {Execution: "idle"}}) {
		t.Fatalf("one swarm: %+v %v", got, err)
	}
}

func TestSwarmSummariesAreOneQuery(t *testing.T) {
	s := summaryStore(t)
	ctx := context.Background()
	db := &countingDB{DB: s.db}
	got, err := swarmSummaries(ctx, db, []string{"s", "busy", "quiet"})
	if err != nil || len(got) != 3 {
		t.Fatalf("summaries: %+v %v", got, err)
	}
	if db.calls != 1 {
		t.Fatalf("three swarms took %d queries, want 1", db.calls)
	}
	db.calls = 0
	got, err = swarmSummaries(ctx, db, nil)
	if err != nil || len(got) != 0 || db.calls != 0 {
		t.Fatalf("empty set: %+v %v queries=%d", got, err, db.calls)
	}
}

// FEATURE: Строка списка роёв несёт определение workflow роя и числа
// объектов, которыми владеет хранилище (#268).
// SPEC: определение — личность снимка, записанная первым запуском (имя,
// версия, ревизия), у роя без неё пусто; участники — явные пары роя и
// пользователя (swarm_members), любой роли; managed-акторы — неудалённые
// (phase не deleted) managed-акторы роя. Счёт — по своему рою, той же
// выборкой, что исполнение и запросы; рой вне набора не читается.
// IMPLEMENTATION: SwarmSummary.Definition*, MemberCount, ManagedActorCount;
// swarmSummaries.
// RESULT: IMPLEMENTED
// STATUS: DONE

// objectStore: рой s запущен из определения ship версии 3, у него три
// участника (owner, editor, viewer) и три managed-актора, один из которых
// удалён; у other — снимок без записанной личности, один участник и один
// managed-актор; bare без определения, участников и акторов.
func objectStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s := swarmTestStore(t, "other", "bare")
	if _, err := s.db.Exec(`UPDATE swarms SET state='active', atespace_uid='native-'||id;
		UPDATE swarms SET definition_name='ship', definition_version=3, definition_revision='rev-3' WHERE id='s';
		UPDATE swarms SET graph=x'00' WHERE id='other';
		INSERT INTO users VALUES ('e','editor',NULL,'h',0,0,'now','now'), ('v','viewer',NULL,'h',0,0,'now','now');
		INSERT INTO swarm_members VALUES ('s','e','editor'), ('s','v','viewer'), ('other','u','owner')`); err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateManagedPool(ctx, ManagedPool{UID: "pool", Name: "pve", Connection: "configured", Source: ManagedSourceSnapshot{ClusterIdentity: "cluster", Endpoint: "https://pve.invalid:8006", Node: "node", ResourcePool: "brood", Storage: "local", Network: "vmbr0"}, Labels: map[string]string{"provider": "proxmox"}, MaxActors: 8, Resources: ManagedResources{CPUMillicores: 8000, MemoryBytes: 8192, DiskBytes: 16384}, Template: ManagedTemplateSnapshot{Profile: "claude-proxmox", URI: "local:template.tar.zst", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, State: "enabled"})
	if err != nil {
		t.Fatal(err)
	}
	for i, a := range []struct{ uid, swarm string }{{"s-new", "s"}, {"s-stopped", "s"}, {"s-gone", "s"}, {"o-new", "other"}} {
		if _, err := s.ReserveManagedActor(ctx, ManagedActor{UID: a.uid, Name: a.uid, SwarmID: a.swarm, AtespaceUID: "native-" + a.swarm, PoolUID: p.UID, Resources: ManagedResources{CPUMillicores: 1000, MemoryBytes: 1024, DiskBytes: 2048}, CTID: 101 + i, OwnerNonce: "owner-" + a.uid, OperationID: "create-" + a.uid}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE managed_actors SET phase='stopped', desired='stopped' WHERE uid='s-stopped';
		UPDATE managed_actors SET phase='deleted', desired='deleted', deleted_at='now' WHERE uid='s-gone'`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSwarmSummariesCarryDefinitionAndStoreCounts(t *testing.T) {
	s := objectStore(t)
	got, err := s.SwarmSummaries(context.Background(), []string{"s", "other", "bare"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]SwarmSummary{
		"s":     {Execution: "idle", DefinitionName: "ship", DefinitionVersion: 3, DefinitionRevision: "rev-3", MemberCount: 3, ManagedActorCount: 2},
		"other": {Execution: "idle", MemberCount: 1, ManagedActorCount: 1},
		"bare":  {Execution: "idle"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summaries:\n got %+v\nwant %+v", got, want)
	}
	// Объекты чужого роя в счёт не входят, и рой вне набора не читается.
	got, err = s.SwarmSummaries(context.Background(), []string{"other"})
	if err != nil || !reflect.DeepEqual(got, map[string]SwarmSummary{"other": want["other"]}) {
		t.Fatalf("one swarm: %+v %v", got, err)
	}
	db := &countingDB{DB: s.db}
	if got, err := swarmSummaries(context.Background(), db, []string{"s", "other", "bare"}); err != nil || len(got) != 3 || db.calls != 1 {
		t.Fatalf("three swarms with objects: %+v %v queries=%d, want 1 query", got, err, db.calls)
	}
}

// countStore: в проекте p рои s (роль u), t (без роли u) и gone (удалён,
// роль u); в проекте q рой x с ролью u, но u в q не участник; проект e пуст.
// adm — администратор без ролей, off — заблокированный администратор,
// vw — участник p без ролей.
func countStore(t *testing.T) *Store {
	t.Helper()
	s := swarmTestStore(t, "t", "gone")
	if _, err := s.db.Exec(`INSERT INTO users VALUES ('adm','adm',NULL,'h',1,0,'now','now');
		INSERT INTO users VALUES ('off','off',NULL,'h',1,1,'now','now');
		INSERT INTO users VALUES ('vw','vw',NULL,'h',0,0,'now','now');
		INSERT INTO projects VALUES ('q','other','now'), ('e','empty','now');
		INSERT INTO project_members VALUES ('p','u'), ('p','vw');
		UPDATE swarms SET state='deleted' WHERE id='gone';
		INSERT INTO swarm_members VALUES ('gone','u','owner');
		INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,created_at,updated_at) VALUES ('x','q','u','x','space-x','now','now');
		INSERT INTO swarm_members VALUES ('x','u','owner')`); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProjectSwarmCountsFollowTheSwarmListRule(t *testing.T) {
	s := countStore(t)
	projects := []string{"p", "q", "e"}
	for _, tc := range []struct {
		user string
		want map[string]int64
	}{
		{"u", map[string]int64{"p": 1, "q": 0, "e": 0}},
		{"adm", map[string]int64{"p": 2, "q": 1, "e": 0}},
		{"vw", map[string]int64{"p": 0, "q": 0, "e": 0}},
		{"off", map[string]int64{"p": 0, "q": 0, "e": 0}},
		{"nobody", map[string]int64{"p": 0, "q": 0, "e": 0}},
	} {
		got, err := s.ProjectSwarmCounts(context.Background(), tc.user, projects)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s counts:\n got %+v\nwant %+v", tc.user, got, tc.want)
		}
	}
}

func TestProjectSwarmCountsAreOneQuery(t *testing.T) {
	s := countStore(t)
	ctx := context.Background()
	db := &countingDB{DB: s.db}
	got, err := projectSwarmCounts(ctx, db, "adm", []string{"p", "q", "e"})
	if err != nil || len(got) != 3 {
		t.Fatalf("counts: %+v %v", got, err)
	}
	if db.calls != 1 {
		t.Fatalf("three projects took %d queries, want 1", db.calls)
	}
	db.calls = 0
	got, err = projectSwarmCounts(ctx, db, "adm", nil)
	if err != nil || len(got) != 0 || db.calls != 0 {
		t.Fatalf("empty set: %+v %v queries=%d", got, err, db.calls)
	}
}
