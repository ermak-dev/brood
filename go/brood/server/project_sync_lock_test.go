package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/app"
)

// FEATURE: Синхронизация capacity разделяет блокировку с admission (#172).
// SPEC: Декларация не обгоняет уже начатую операцию с ёмкостью проекта.
func TestProjectSyncSharesCapacityAdmissionLock(t *testing.T) {
	ts, admin, _, _, id := gitRepositorySetup(t)
	user, err := ts.srv.store.GetUserByUsername(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	interleaved := &interleavedCapacityStore{Store: ts.srv.gateway, beforeSet: func() {
		close(entered)
		<-release
	}}
	ts.srv.app = app.NewService(interleaved, ts.provisioner, time.Now, nil)
	older := make(chan error, 1)
	go func() {
		older <- ts.srv.app.SetShardCapacity(user.ID, id, app.ShardCapacity{WorkerSelector: map[string]string{"pool": "older"}})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("older capacity operation did not acquire admission lock")
	}
	body := `{"name":"repo-project","repository":"","capacity":{"worker_selector":{"pool":"declaration"},"max_actors":"3"}}`
	req, err := http.NewRequest(http.MethodPut, ts.http.URL+"/api/projects/sync", strings.NewReader(body))
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", ts.http.URL)
	req.Header.Set("X-CSRFToken", ts.csrf(admin))
	completed := make(chan int, 1)
	go func() {
		response, err := admin.Do(req)
		if err != nil {
			completed <- 0
			return
		}
		response.Body.Close()
		completed <- response.StatusCode
	}()
	escaped, status := false, 0
	select {
	case status = <-completed:
		escaped = true
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-older; err != nil {
		t.Fatal(err)
	}
	if !escaped {
		select {
		case status = <-completed:
		case <-time.After(5 * time.Second):
			t.Fatal("declaration did not resume after admission lock release")
		}
	}
	if escaped {
		t.Fatalf("project sync bypassed capacity admission lock: status %d", status)
	}
	if status != http.StatusOK {
		t.Fatalf("sync after lock release: %d", status)
	}
	capacity, err := ts.srv.store.GetProjectCapacity(context.Background(), id)
	if err != nil || capacity.WorkerSelector["pool"] != "declaration" || capacity.MaxActors == nil || *capacity.MaxActors != 3 {
		t.Fatalf("capacity update order was lost: %+v %v", capacity, err)
	}
}
