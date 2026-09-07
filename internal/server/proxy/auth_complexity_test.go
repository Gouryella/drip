package proxy

import (
	"sync"
	"testing"
	"time"
)

func TestAuthSessionStoreExpirationRemovesQueueEntries(t *testing.T) {
	store := &authSessionStore{}
	now := time.Now()
	store.addSessionLocked("oldest", &authSession{subdomain: "demo", expiresAt: now.Add(-time.Minute)})
	store.addSessionLocked("expired", &authSession{subdomain: "demo", expiresAt: now.Add(-time.Second)})
	store.addSessionLocked("live", &authSession{subdomain: "demo", expiresAt: now.Add(time.Hour)})
	if store.validate("expired", "demo") {
		t.Fatal("expired session was accepted")
	}
	if store.expiryOrder.Len() != 2 {
		t.Fatal("validation left an expired queue entry")
	}
	store.cleanupExpiredLocked(now)
	if len(store.sessions) != 1 || store.expiryOrder.Len() != 1 {
		t.Fatal("expiration left stale entries")
	}
	if !store.validate("live", "demo") {
		t.Fatal("cleanup removed a live session")
	}
	store.removeSessionLocked("live")
	if store.expiryOrder.Len() != 0 {
		t.Fatal("removal retained the session")
	}
}

func TestConcurrentAuthSessionCreationPreservesExpiryOrder(t *testing.T) {
	store := &authSessionStore{}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token := store.create("demo", "generation")
			if !store.validate(token, "demo", "generation") {
				t.Error("new session was rejected")
			}
		}()
	}
	wg.Wait()
	if len(store.sessions) != 64 || store.expiryOrder.Len() != 64 {
		t.Fatal("session indexes disagree")
	}
	var previous time.Time
	for e := store.expiryOrder.Front(); e != nil; e = e.Next() {
		expires := e.Value.(*authSession).expiresAt
		if expires.Before(previous) {
			t.Fatal("FIFO expiration order was lost")
		}
		previous = expires
	}
}

func BenchmarkAuthSessionCreateAtCapacity(b *testing.B) {
	store := &authSessionStore{sessions: make(map[string]*authSession, maxAuthSessions)}
	for i := 0; i < maxAuthSessions; i++ {
		store.create("benchmark")
	}
	b.ReportAllocs()
	for b.Loop() {
		if token := store.create("benchmark"); token == "" {
			b.Fatal("failed to create session at capacity")
		}
	}
}
