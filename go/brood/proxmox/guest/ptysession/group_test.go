//go:build linux

package ptysession

import "testing"

// FEATURE: Подтверждение выхода process group до освобождения PID лидера.
// SPEC: Зомби не исполняются; живой участник группы блокирует завершение.
func TestLiveProcessGroup(t *testing.T) {
	for _, tt := range []struct {
		name, stat    string
		group         int
		live, invalid bool
	}{
		{"live", "21 (worker (job)) S 1 17 17", 17, true, false},
		{"other group", "21 (worker) S 1 18 18", 17, false, false},
		{"zombie", "21 (worker) Z 1 17 17", 17, false, false},
		{"dead", "21 (worker) X 1 17 17", 17, false, false},
		{"invalid", "21 worker S 1 17", 17, false, true},
		{"short", "21 (worker) S 1", 17, false, true},
		{"invalid group", "21 (worker) S 1 bad 17", 17, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			live, err := liveProcessGroup([]byte(tt.stat), tt.group)
			if (err != nil) != tt.invalid || live != tt.live {
				t.Fatalf("live=%v err=%v", live, err)
			}
		})
	}
}
