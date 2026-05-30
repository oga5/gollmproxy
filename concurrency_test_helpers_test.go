package main

import (
	"testing"
	"time"
)

func waitForQueuedRequests(t *testing.T, ctrl *KeyedConcurrencyController, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		ctrl.mu.Lock()
		got := 0
		if st := ctrl.perKey[key]; st != nil {
			got = st.waiting
		}
		ctrl.mu.Unlock()

		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waiting count for key %q did not become %d", key, want)
}
