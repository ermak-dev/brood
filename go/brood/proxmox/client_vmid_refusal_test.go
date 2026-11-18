package proxmox

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// BUG: Отказ глобального exact-VMID запроса неотличим от недоступности провайдера.
// SPEC: 400/409 при проверке конкретного ID — отказ кандидату, никогда не absence;
// permission/server failure и отказ обычному NextID не разрешают перебор Create.
// SOLUTION: HTTP status сохраняется типом; только exact-ID 400 нормализуется в collision.
// RESULT: FIXED — один read, ноль writes, ошибки не выдают positive absence.
// STATUS: FIXED
func TestGlobalVMIDCandidateRefusalIsNotAbsence(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 500, 502} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			reads := 0
			client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				reads++
				if r.Method != "GET" || r.URL.Path != "/api2/json/cluster/nextid" || r.URL.RawQuery != "vmid=123" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
				}
				w.WriteHeader(status)
			})
			err := client.ConfirmAbsent(context.Background(), 123)
			if err == nil {
				t.Fatal("rejected VMID became positive absence")
			}
			if errors.Is(err, ErrCollision) != (status == 400 || status == 409) {
				t.Fatalf("candidate refusal status %d: %v", status, err)
			}
			if errors.Is(err, ErrPermission) != (status == 401 || status == 403) {
				t.Fatalf("permission classification %d: %v", status, err)
			}
			if reads != 1 {
				t.Fatalf("candidate check retried or wrote work: %d", reads)
			}
		})
	}
	client, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.RawQuery != "" {
			t.Errorf("unexpected suggestion request: %s %s", r.Method, r.URL.String())
		}
		w.WriteHeader(http.StatusBadRequest)
	})
	if _, err := client.NextID(context.Background()); err == nil || errors.Is(err, ErrCollision) {
		t.Fatalf("ordinary suggestion failure became an exact-ID collision: %v", err)
	}
}
