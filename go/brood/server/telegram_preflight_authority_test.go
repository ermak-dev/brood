package server

// BUG: Отозванный во время preflight чат получает предупреждение без исполнения (#299).
// HYPOTHESIS: Класс ошибки namespace не доказывает адаптеру, что исполнитель не вызывался.
// SOLUTION: Каталог явно доказывает отсутствие намерения при отказе preflight; отозванному чату нет ответа.
// RESULT: FIXED — оба отказа не отправляют сообщения, не меняют актора и не пишут провайдеру.
// STATUS: FIXED

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc/codes"
)

func TestTelegramRevokedPreflightFailureIsSilent(t *testing.T) {
	for _, failure := range []string{"namespace mismatch", "namespace read failure"} {
		t.Run(failure, func(t *testing.T) {
			f := telegramActorSetup(t)
			ask := f.chat.confirmation("/actor alpha coding stop")
			before := f.actor(t)
			writes := lifecycleProviderWrites(f.pve)
			sw, err := f.ts.srv.store.GetSwarm(context.Background(), f.swarm)
			if err != nil {
				t.Fatal(err)
			}
			var hooks hookErrors
			called := false
			f.ts.fake.mu.Lock()
			f.ts.fake.beforeNamespaceGet = func() {
				called = true
				f.unlink(&hooks)()
				if failure == "namespace mismatch" {
					f.ts.fake.atespaces[sw.AtespaceName].Metadata.Uid = "replacement-namespace"
				} else {
					f.ts.fake.namespaceError = grpcErr(codes.Unavailable, "test namespace unavailable")
				}
			}
			f.ts.fake.mu.Unlock()
			got := f.chat.press("q-preflight", ask, 0)
			hooks.check(t)
			after := f.actor(t)
			if !called || !reflect.DeepEqual(after, before) || lifecycleProviderWrites(f.pve) != writes {
				t.Fatalf("invalid no-intent proof: called=%v revision=%d -> %d writes=%v -> %v", called, before.Revision, after.Revision, writes, lifecycleProviderWrites(f.pve))
			}
			if len(got.sent) != 0 {
				t.Fatalf("revoked chat received preflight-only reply: sent=%+v revision=%d -> %d writes=%v -> %v", got.sent, before.Revision, after.Revision, writes, lifecycleProviderWrites(f.pve))
			}
		})
	}
}
