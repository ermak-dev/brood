package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// FEATURE: Default swarm of a linked Telegram chat (#284, epic #278).
// SPEC: /use keeps one swarm in the user's link row as the context of the
// chat's commands. The write checks, in its own transaction, the telegram
// session of the update (authorize) and that the user still reads the swarm
// by the swarm view rule: an enabled administrator, or a project member with
// a role in the swarm, and a swarm that is not deleted. A refusal writes
// nothing: a chat that is not linked gets ErrTelegramUnlinked, a swarm the
// user does not read is not found, as if it did not exist. The default
// survives a reopen of the store; deleting the swarm row clears it.
// IMPLEMENTATION: user_telegram_links.default_swarm_id (schema 30,
// telegram_schema.go), SetTelegramSwarm (telegram_links.go).
// STATUS: DONE

// swarmLinkStore: requestStore с привязанными чатами: u (owner) — 41, ed
// (editor) — 42, vw (viewer) — 43, out (вне проекта) — 44, adm
// (администратор без членства) — 45, все через бот установки.
func swarmLinkStore(t *testing.T) *Store {
	t.Helper()
	s, _ := requestStore(t)
	for i, user := range []string{"u", "ed", "vw", "out", "adm"} {
		linkTelegram(t, s, user, "", int64(41+i))
	}
	return s
}

// defaultSwarm — рой по умолчанию в строке привязки пользователя.
func defaultSwarm(t *testing.T, s *Store, user string) string {
	t.Helper()
	link, err := s.GetTelegramLink(context.Background(), user)
	mustNil(t, err)
	return link.DefaultSwarmID
}

func TestTelegramDefaultSwarmIsKeptInTheLinkRow(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/brood.db"
	s, err := Open(ctx, path)
	mustNil(t, err)
	seedUser(t, s, "u", "user")
	seedProject(t, s, "p", "project")
	mustNil(t, s.AddMember(ctx, "p", "u"))
	mustNil(t, s.CreateSwarm(ctx, Swarm{ID: "s", ProjectID: "p", OwnerID: "u", Name: "alpha", AtespaceName: "space", State: "active"}))
	linkTelegram(t, s, "u", "", 41)
	link, err := s.SetTelegramSwarm(ctx, "u", "s", allow)
	mustNil(t, err)
	if link.DefaultSwarmID != "s" || link.State != "linked" || link.ChatID != 41 {
		t.Fatalf("link after /use = %+v", link)
	}
	mustNil(t, s.Close())
	s, err = Open(ctx, path)
	mustNil(t, err)
	defer s.Close()
	if got := defaultSwarm(t, s, "u"); got != "s" {
		t.Fatalf("default swarm after reopen = %q, want s", got)
	}
	// Другой рой заменяет прежний: рой по умолчанию один.
	mustNil(t, s.CreateSwarm(ctx, Swarm{ID: "s2", ProjectID: "p", OwnerID: "u", Name: "beta", AtespaceName: "space-2", State: "active"}))
	if _, err := s.SetTelegramSwarm(ctx, "u", "s2", allow); err != nil {
		t.Fatal(err)
	}
	if got := defaultSwarm(t, s, "u"); got != "s2" {
		t.Fatalf("default swarm after a second /use = %q, want s2", got)
	}
}

func TestTelegramDefaultSwarmFollowsTheSwarmReadRule(t *testing.T) {
	ctx := context.Background()
	s := swarmLinkStore(t)
	// Читатель роя — owner, editor, viewer и администратор без членства.
	for _, user := range []string{"u", "ed", "vw", "adm"} {
		if _, err := s.SetTelegramSwarm(ctx, user, "s", allow); err != nil {
			t.Fatalf("%s reads the swarm, /use refused: %v", user, err)
		}
		if got := defaultSwarm(t, s, user); got != "s" {
			t.Fatalf("%s default swarm = %q", user, got)
		}
	}
	// Вне проекта, без роли, неизвестный и удалённый рой — не найден.
	mustNil(t, s.AddMember(ctx, "p", "out"))
	_, err := s.db.Exec(`INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,state,created_at,updated_at) VALUES ('gone','p','u','gone','space-gone','deleted','now','now');
		INSERT INTO swarm_members VALUES ('gone','u','owner')`)
	mustNil(t, err)
	for _, tc := range []struct{ user, swarm string }{{"out", "s"}, {"u", "missing"}, {"u", "gone"}, {"adm", "gone"}} {
		before, err := s.GetTelegramLink(ctx, tc.user)
		mustNil(t, err)
		if _, err := s.SetTelegramSwarm(ctx, tc.user, tc.swarm, allow); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s /use %s = %v, want not found", tc.user, tc.swarm, err)
		}
		if after, err := s.GetTelegramLink(ctx, tc.user); err != nil || !after.equal(before) {
			t.Fatalf("a refused /use changed the link of %s: %+v -> %+v (%v)", tc.user, before, after, err)
		}
	}
	// Отключённый пользователь роя не читает, даже с ролью в нём.
	mustNil(t, s.SetUserDisabled(ctx, "ed", true))
	if _, err := s.SetTelegramSwarm(ctx, "ed", "s", allow); err == nil {
		t.Fatal("a disabled editor changed the default swarm")
	}
}

func (l TelegramLink) equal(other TelegramLink) bool {
	return l.UserID == other.UserID && l.DefaultSwarmID == other.DefaultSwarmID && l.State == other.State && l.UpdatedAt == other.UpdatedAt
}

func TestTelegramDefaultSwarmNeedsALiveLink(t *testing.T) {
	ctx := context.Background()
	s := swarmLinkStore(t)
	seedUser(t, s, "new", "newcomer")
	mustNil(t, s.AddMember(ctx, "p", "new"))
	_, err := s.db.Exec(`INSERT INTO swarm_members VALUES ('s','new','editor')`)
	mustNil(t, err)
	deny := func(context.Context) bool { return false }
	before, err := s.GetTelegramLink(ctx, "u")
	mustNil(t, err)
	if _, err := s.SetTelegramSwarm(ctx, "u", "s", deny); !errors.Is(err, ErrTelegramUnlinked) {
		t.Fatalf("/use with a dead session = %v, want ErrTelegramUnlinked", err)
	}
	if after, _ := s.GetTelegramLink(ctx, "u"); !after.equal(before) {
		t.Fatalf("a refused /use changed the link: %+v -> %+v", before, after)
	}
	// Без привязки строки нет, и отказ её не заводит.
	if _, err := s.SetTelegramSwarm(ctx, "new", "s", allow); !errors.Is(err, ErrTelegramUnlinked) {
		t.Fatalf("/use of a user without a link = %v, want ErrTelegramUnlinked", err)
	}
	var rows int
	mustNil(t, s.db.QueryRow(`SELECT count(*) FROM user_telegram_links WHERE user_id = 'new'`).Scan(&rows))
	if rows != 0 {
		t.Fatal("a refused /use wrote a link row")
	}
	// Сессию authorize видит в транзакции записи.
	var inTx bool
	if _, err := s.SetTelegramSwarm(ctx, "u", "s", func(ctx context.Context) bool {
		_, inTx = ctx.Value(txKey{}).(*sql.Tx)
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if !inTx {
		t.Fatal("authorize ran outside the write transaction")
	}
}

func TestTelegramDefaultSwarmIsClearedWithItsSwarm(t *testing.T) {
	ctx := context.Background()
	s := swarmLinkStore(t)
	_, err := s.db.Exec(`INSERT INTO swarms (id,project_id,owner_id,name,atespace_name,state,created_at,updated_at) VALUES ('s2','p','u','beta','space-2','active','now','now');
		INSERT INTO swarm_members VALUES ('s2','u','owner')`)
	mustNil(t, err)
	if _, err := s.SetTelegramSwarm(ctx, "u", "s2", allow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTelegramSwarm(ctx, "vw", "s", allow); err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`DELETE FROM swarm_members WHERE swarm_id = 's2'; DELETE FROM swarms WHERE id = 's2'`)
	mustNil(t, err)
	if got := defaultSwarm(t, s, "u"); got != "" {
		t.Fatalf("default swarm of a deleted swarm row = %q", got)
	}
	if link, err := s.GetTelegramLink(ctx, "u"); err != nil || link.State != "linked" {
		t.Fatalf("the link after its default swarm was deleted = %+v, %v", link, err)
	}
	if got := defaultSwarm(t, s, "vw"); got != "s" {
		t.Fatalf("another user's default swarm = %q", got)
	}
}
