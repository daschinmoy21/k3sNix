package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// With the limit at N, N+1 distinct slow dumps never run at once.
func TestMaxConcurrentDumps(t *testing.T) {
	dir := t.TempDir()
	const limit = 3
	paths := make([]string, limit+1)
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("%032d-pkg", i))
		mustDir(t, paths[i])
	}
	s := newServer(t, dir)
	s.MaxConcurrentDumps = limit

	var active, peak atomic.Int32
	gate := make(chan struct{})
	s.DumpFunc = func(path string, w io.Writer) error {
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-gate
		_, err := w.Write([]byte("nar"))
		return err
	}

	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := s.dumpCoalesced(context.Background(), p)
			release()
			if err != nil {
				t.Error(err)
			}
		}()
	}
	waitFor(t, func() bool { return active.Load() == limit })
	time.Sleep(50 * time.Millisecond) // give an unlimited N+1th dump the chance to start
	close(gate)
	wg.Wait()
	if got := peak.Load(); got != limit {
		t.Fatalf("peak concurrent dumps = %d, want %d", got, limit)
	}
}

// Waiters for a path that is already being dumped do not take a slot.
func TestCoalescedWaitersShareOneSlot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pkg")
	mustDir(t, p)
	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1
	s.DumpWaitTimeout = 20 * time.Millisecond // a waiter taking a slot would fail fast

	var dumps atomic.Int32
	gate := make(chan struct{})
	s.DumpFunc = func(path string, w io.Writer) error {
		dumps.Add(1)
		<-gate
		_, err := w.Write([]byte("nar"))
		return err
	}

	const callers = 5
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := s.dumpCoalesced(context.Background(), p)
			release()
			if err != nil {
				t.Error(err)
			}
		}()
	}
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		c := s.dumps[p]
		return c != nil && c.refs == callers
	})
	time.Sleep(2 * s.DumpWaitTimeout) // outlive the slot wait while the leader holds the slot
	close(gate)
	wg.Wait()
	if got := dumps.Load(); got != 1 {
		t.Fatalf("DumpFunc ran %d times, want 1", got)
	}
}

// A request that cannot get a slot within DumpWaitTimeout is answered 503.
func TestDumpSlotTimeoutIs503(t *testing.T) {
	dir := t.TempDir()
	busy := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-busy")
	other := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-other")
	mustDir(t, busy)
	mustDir(t, other)
	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1
	s.DumpWaitTimeout = 20 * time.Millisecond

	started, gate := make(chan struct{}), make(chan struct{})
	s.DumpFunc = func(path string, w io.Writer) error {
		if path == busy {
			close(started)
			<-gate
		}
		_, err := w.Write([]byte("nar"))
		return err
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, release, err := s.dumpCoalesced(context.Background(), busy)
		release()
		if err != nil {
			t.Error(err)
		}
	}()
	<-started

	for _, url := range []string{
		"/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.narinfo",
		"/nar/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.nar",
	} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", url, rec.Code)
		}
	}
	close(gate)
	<-done
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for condition")
		}
		time.Sleep(time.Millisecond)
	}
}
