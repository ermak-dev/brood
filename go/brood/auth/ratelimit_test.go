package auth

import (
	"testing"
	"time"
)

func TestLimiterAllowsUnderLimit(t *testing.T) {
	l := NewLoginLimiter(3, 10*time.Minute)
	now := time.Now()
	for i := 0; i < 3; i++ {
		if !l.Allowed("alice", "1.2.3.4", now) {
			t.Fatalf("attempt %d must be allowed", i)
		}
		l.RecordFail("alice", "1.2.3.4", now)
	}
	if l.Allowed("alice", "1.2.3.4", now) {
		t.Fatal("4th attempt must be refused")
	}
}

func TestLimiterSeparateKeys(t *testing.T) {
	l := NewLoginLimiter(2, 10*time.Minute)
	now := time.Now()
	l.RecordFail("alice", "1.2.3.4", now)
	l.RecordFail("alice", "1.2.3.4", now)
	// Username-счётчик полон, другой IP с тем же именем — отказ.
	if l.Allowed("alice", "5.6.7.8", now) {
		t.Fatal("username counter must refuse other ip")
	}
	// Другое имя с того же IP — IP-счётчик ещё не полон (2 из 2 — полон).
	// После двух неудач alice IP-счётчик тоже на пределе.
	if l.Allowed("bob", "1.2.3.4", now) {
		t.Fatal("ip counter must refuse other username")
	}
	l2 := NewLoginLimiter(2, 10*time.Minute)
	l2.RecordFail("alice", "1.2.3.4", now)
	if !l2.Allowed("bob", "1.2.3.4", now) {
		t.Fatal("one fail must not block ip counter")
	}
}

func TestLimiterUsernameCaseInsensitive(t *testing.T) {
	l := NewLoginLimiter(1, 10*time.Minute)
	now := time.Now()
	l.RecordFail("Alice", "1.2.3.4", now)
	if l.Allowed("alice", "5.6.7.8", now) {
		t.Fatal("case change must not bypass username counter")
	}
}

func TestLimiterWindowSlides(t *testing.T) {
	l := NewLoginLimiter(2, 10*time.Minute)
	now := time.Now()
	l.RecordFail("alice", "1.2.3.4", now.Add(-11*time.Minute))
	l.RecordFail("alice", "1.2.3.4", now.Add(-11*time.Minute))
	if !l.Allowed("alice", "1.2.3.4", now) {
		t.Fatal("expired failures must leave the window")
	}
}

func TestLimiterResetOnSuccess(t *testing.T) {
	l := NewLoginLimiter(2, 10*time.Minute)
	now := time.Now()
	l.RecordFail("alice", "1.2.3.4", now)
	l.RecordFail("alice", "1.2.3.4", now)
	if l.Allowed("alice", "1.2.3.4", now) {
		t.Fatal("must be limited before reset")
	}
	l.Reset("alice", "1.2.3.4")
	if !l.Allowed("alice", "1.2.3.4", now) {
		t.Fatal("reset must clear counters")
	}
}

func TestLimiterGC(t *testing.T) {
	l := NewLoginLimiter(1, time.Minute)
	now := time.Now()
	l.RecordFail("alice", "1.2.3.4", now)
	l.RecordFail("bob", "5.6.7.8", now)
	// gcEvery — минута: к этому моменту оба счётчика устарели и должны
	// уйти из карты.
	l.Allowed("carol", "9.9.9.9", now.Add(2*time.Minute))
	l.mu.Lock()
	n := len(l.fails)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("gc must drop stale counters, %d left", n)
	}
}
