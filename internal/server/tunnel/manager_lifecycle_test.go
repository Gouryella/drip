package tunnel

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"
)

func TestShutdownRejectsRegistrationAndResetsCounters(t *testing.T) {
	m := NewManager(zap.NewNop())
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, _ = m.Register(nil, fmt.Sprintf("shutdown-%d", i)) }(i)
	}
	m.Shutdown()
	wg.Wait()
	if _, err := m.Register(nil, "after-shutdown"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Register after Shutdown = %v", err)
	}
	m.Unregister("shutdown-0")
	if m.Count() != 0 || len(m.List()) != 0 {
		t.Fatalf("shutdown left count=%d, list=%d", m.Count(), len(m.List()))
	}
}

func TestOldRegistrationCleanupPreservesReplacement(t *testing.T) {
	m := NewManager(zap.NewNop())
	defer m.Shutdown()
	if _, err := m.Register(nil, "reused"); err != nil {
		t.Fatal(err)
	}
	old, _ := m.Get("reused")
	m.Unregister("reused")
	if _, err := m.Register(nil, "reused"); err != nil {
		t.Fatal(err)
	}
	replacement, _ := m.Get("reused")
	m.UnregisterIf("reused", old)
	if got, ok := m.Get("reused"); !ok || got != replacement || got.IsClosed() {
		t.Fatal("old cleanup removed the new tunnel")
	}
}
