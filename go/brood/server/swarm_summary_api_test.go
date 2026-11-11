package server

// FEATURE: Строка списка роёв несёт исполнение и число открытых запросов,
// строка списка проектов — число роёв (#234).
// SPEC: GET /api/projects/{id}/swarms отдаёт у каждой строки `execution`
// (swarms.execution) и `openRequests` (открытые запросы роя, узловые и
// актора); GET /api/projects — `swarmCount`, равный длине списка роёв
// проекта у того же пользователя. Права — те же, что у списков: сводка не
// показывает рой, которого вызывающий в списке не видит, и рой другого
// проекта в список не попадает.
// IMPLEMENTATION: writeSwarms и writeProjects берут сводку одной выборкой
// хранилища (store.SwarmSummaries, store.ProjectSwarmCounts).
// RESULT: IMPLEMENTED
// STATUS: DONE

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
)

// execFixture пишет строки напрямую в базу сервера: тест провода читает
// сводку, а не производственный путь к ждущему графу и вопросам актора.
func execFixture(t *testing.T, ts *testServer, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", ts.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `PRAGMA foreign_keys=ON; PRAGMA busy_timeout=10000`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// listRows читает массив строк ответа списка.
func listRows(t *testing.T, ts *testServer, client *http.Client, path string) []map[string]any {
	t.Helper()
	r, _, raw := ts.do(client, "GET", path, "", nil)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, r.StatusCode, raw)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(raw), &rows); err != nil || rows == nil {
		t.Fatalf("GET %s: %s %v", path, raw, err)
	}
	return rows
}

// summaryOf: исполнение и открытые запросы строки по ID роя; nil — строки нет.
func summaryOf(rows []map[string]any, id string) []any {
	for _, row := range rows {
		if row["id"] == id {
			return []any{row["execution"], row["openRequests"]}
		}
	}
	return nil
}

func TestSwarmListCarriesExecutionAndOpenRequests(t *testing.T) {
	ts := newTestServer(t)
	ts.bootstrapAdmin("root", "root-password-1")
	root, alice, bob := newClient(t), newClient(t), newClient(t)
	ts.login(root, "root", "root-password-1")
	create := func(client *http.Client, path string, body map[string]any) string {
		t.Helper()
		r, b, _ := ts.do(client, "POST", path, ts.csrf(client), body)
		expectStatus(t, "POST "+path, r, b, http.StatusCreated)
		id, _ := b["id"].(string)
		return id
	}
	for _, name := range []string{"alice", "bob"} {
		create(root, "/api/admin/users", map[string]any{"username": name, "password": name + "-password-1"})
	}
	project := create(root, "/api/projects", map[string]any{"name": "proj"})
	side := create(root, "/api/projects", map[string]any{"name": "side"})
	for _, name := range []string{"alice", "bob"} {
		r, b, _ := ts.do(root, "POST", "/api/projects/"+project+"/members", ts.csrf(root), map[string]string{"username": name})
		expectStatus(t, "member "+name, r, b, http.StatusNoContent)
	}
	ts.login(alice, "alice", "alice-password-1")
	ts.login(bob, "bob", "bob-password-1")
	alpha := create(alice, "/api/projects/"+project+"/swarms", map[string]any{"name": "alpha"})
	beta := create(bob, "/api/projects/"+project+"/swarms", map[string]any{"name": "beta"})
	gamma := create(root, "/api/projects/"+side+"/swarms", map[string]any{"name": "gamma"})
	doomed := create(alice, "/api/projects/"+project+"/swarms", map[string]any{"name": "doomed"})
	r, b, _ := ts.do(alice, "DELETE", "/api/swarms/"+doomed, ts.csrf(alice), nil)
	expectStatus(t, "delete doomed", r, b, http.StatusNoContent)
	aliceUser, err := ts.srv.store.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}

	// alpha ждёт оператора: узловой запрос и вопрос актора открыты, второй
	// вопрос отвечен, третий отменён. beta идёт с одним вопросом актора,
	// gamma другого проекта встал с тремя.
	for swarm, execution := range map[string]string{alpha: "waiting", beta: "running", gamma: "failed"} {
		execFixture(t, ts, `UPDATE swarms SET execution=? WHERE id=?`, execution, swarm)
	}
	execFixture(t, ts, `INSERT INTO swarm_steps(swarm_id,sequence,node_id,state) VALUES (?,1,'ask','waiting')`, alpha)
	execFixture(t, ts, `INSERT INTO swarm_requests(id,swarm_id,sequence,origin,kind,question,options,state,created_at) VALUES ('node',?,1,'node','choice','Ship?',x'5b5d','open','now')`, alpha)
	for _, row := range []struct{ id, swarm, state string }{
		{"a-open", alpha, "open"}, {"a-cancelled", alpha, "cancelled"},
		{"b-open", beta, "open"},
		{"g-1", gamma, "open"}, {"g-2", gamma, "open"}, {"g-3", gamma, "open"},
	} {
		execFixture(t, ts, `INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at) VALUES (?,?,'actor','worker',1,'text','q',?,'now')`, row.id, row.swarm, row.state)
	}
	execFixture(t, ts, `INSERT INTO swarm_requests(id,swarm_id,origin,actor_uid,actor_incarnation,kind,question,state,created_at,answered_at,answered_by,response,delivery) VALUES ('a-answered',?,'actor','worker',1,'text','q','answered','now','now',?,'ok','delivered')`, alpha, aliceUser.ID)

	swarms := "/api/projects/" + project + "/swarms"
	for _, tc := range []struct {
		who    string
		client *http.Client
		want   map[string][]any
	}{
		{"root", root, map[string][]any{alpha: {"waiting", float64(2)}, beta: {"running", float64(1)}}},
		{"alice", alice, map[string][]any{alpha: {"waiting", float64(2)}}},
		{"bob", bob, map[string][]any{beta: {"running", float64(1)}}},
	} {
		rows := listRows(t, ts, tc.client, swarms)
		if len(rows) != len(tc.want) {
			t.Fatalf("%s sees %d swarms, want %d: %v", tc.who, len(rows), len(tc.want), rows)
		}
		for id, want := range tc.want {
			if got := summaryOf(rows, id); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
				t.Fatalf("%s row %s: execution, openRequests = %v, want %v", tc.who, id, got, want)
			}
		}
		for _, hidden := range []string{gamma, doomed} {
			if summaryOf(rows, hidden) != nil {
				t.Fatalf("%s lists swarm %s outside the project list: %v", tc.who, hidden, rows)
			}
		}
	}

	// Число роёв проекта — длина его списка роёв у того же пользователя.
	for _, tc := range []struct {
		who    string
		client *http.Client
		want   map[string]float64
	}{
		{"root", root, map[string]float64{project: 2, side: 1}},
		{"alice", alice, map[string]float64{project: 1}},
		{"bob", bob, map[string]float64{project: 1}},
	} {
		projects := listRows(t, ts, tc.client, "/api/projects")
		if len(projects) != len(tc.want) {
			t.Fatalf("%s sees projects %v, want %v", tc.who, projects, tc.want)
		}
		for _, row := range projects {
			id, _ := row["id"].(string)
			listed := listRows(t, ts, tc.client, "/api/projects/"+id+"/swarms")
			if row["swarmCount"] != tc.want[id] || row["swarmCount"] != float64(len(listed)) {
				t.Fatalf("%s project %s: swarmCount %v, want %v, list has %d", tc.who, id, row["swarmCount"], tc.want[id], len(listed))
			}
		}
	}
}
