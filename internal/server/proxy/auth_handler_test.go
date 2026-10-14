package proxy

import (
	"fmt"
	"testing"
	"time"
)

func TestAuthSessionStoreCreateEvictsOldestSessionWhenFull(t *testing.T) {
	t.Parallel()

	store := &authSessionStore{
		sessions: make(map[string]*authSession, maxAuthSessions),
	}

	base := time.Now().Add(5 * time.Minute)
	for i := 0; i < maxAuthSessions; i++ {
		store.addSessionLocked(fmt.Sprintf("token-%05d", i), &authSession{
			subdomain: "demo",
			expiresAt: base.Add(time.Duration(i) * time.Millisecond),
		})
	}

	token := store.create("demo")
	if token == "" {
		t.Fatal("create() returned empty token")
	}
	if len(store.sessions) != maxAuthSessions {
		t.Fatalf("len(store.sessions) = %d, want %d", len(store.sessions), maxAuthSessions)
	}
	if _, ok := store.sessions["token-00000"]; ok {
		t.Fatal("oldest session was not evicted")
	}
	if _, ok := store.sessions[token]; !ok {
		t.Fatal("new session token was not stored")
	}
}

func TestAuthSessionStoreCreatePrunesExpiredSessionsBeforeEviction(t *testing.T) {
	t.Parallel()

	store := &authSessionStore{
		sessions: make(map[string]*authSession, maxAuthSessions),
	}

	now := time.Now()
	store.addSessionLocked("expired", &authSession{
		subdomain: "demo",
		expiresAt: now.Add(-1 * time.Minute),
	})
	for i := 0; i < maxAuthSessions-1; i++ {
		store.addSessionLocked(fmt.Sprintf("token-%05d", i), &authSession{
			subdomain: "demo",
			expiresAt: now.Add(10*time.Minute + time.Duration(i)*time.Millisecond),
		})
	}

	token := store.create("demo")
	if token == "" {
		t.Fatal("create() returned empty token")
	}
	if _, ok := store.sessions["expired"]; ok {
		t.Fatal("expired session was not pruned")
	}
	if len(store.sessions) != maxAuthSessions {
		t.Fatalf("len(store.sessions) = %d, want %d", len(store.sessions), maxAuthSessions)
	}
	if _, ok := store.sessions[token]; !ok {
		t.Fatal("new session token was not stored")
	}
}
