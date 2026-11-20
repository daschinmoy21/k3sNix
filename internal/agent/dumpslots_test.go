package agent

import (
	"context"
	"errors"
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

// A leader whose request ends while it waits for a slot still dumps for
// the waiters that joined it.
func TestLeaderCancelDoesNotFailWaiters(t *testing.T) {
	dir := t.TempDir()
	busy := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-busy")
	p := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-pkg")
	mustDir(t, busy)
	mustDir(t, p)
	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1

	started, gate := make(chan struct{}), make(chan struct{})
	s.DumpFunc = func(path string, w io.Writer) error {
		if path == busy {
			close(started)
			<-gate
		}
		_, err := w.Write([]byte("nar"))
		return err
	}
	busyDone := make(chan struct{})
	go func() {
		defer close(busyDone)
		_, release, _ := s.dumpCoalesced(context.Background(), busy)
		release()
	}()
	<-started

	// The leader for p queues behind busy, then its request is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, release, err := s.dumpCoalesced(ctx, p)
		release()
		leaderErr <- err
	}()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.dumps[p] != nil
	})
	waiterErr := make(chan error, 1)
	go func() {
		_, release, err := s.dumpCoalesced(context.Background(), p)
		release()
		waiterErr <- err
	}()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.dumps[p].refs == 2
	})
	cancel()
	close(gate)
	if err := <-waiterErr; err != nil {
		t.Fatalf("waiter: %v, want the leader's dump", err)
	}
	if err := <-leaderErr; err != nil {
		t.Fatalf("cancelled leader: %v, want its dump to finish", err)
	}
	<-busyDone
}

// A DumpFunc panic gives the slot back and fails the waiters instead of
// leaving them blocked.
func TestDumpPanicReleasesSlot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pkg")
	mustDir(t, p)
	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1
	s.DumpWaitTimeout = 50 * time.Millisecond

	var calls atomic.Int32
	gate := make(chan struct{})
	s.DumpFunc = func(path string, w io.Writer) error {
		if calls.Add(1) == 1 {
			<-gate
			panic("boom")
		}
		_, err := w.Write([]byte("nar"))
		return err
	}
	leaderDone := make(chan any, 1)
	go func() {
		defer func() { leaderDone <- recover() }()
		s.dumpCoalesced(context.Background(), p)
	}()
	waitFor(t, func() bool { return calls.Load() == 1 })
	waiterErr := make(chan error, 1)
	go func() {
		_, release, err := s.dumpCoalesced(context.Background(), p)
		release()
		waiterErr <- err
	}()
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		c := s.dumps[p]
		return c != nil && c.refs == 2
	})
	close(gate)
	if r := <-leaderDone; r == nil {
		t.Fatal("leader did not panic")
	}
	if err := <-waiterErr; !errors.Is(err, errDumpPanicked) {
		t.Fatalf("waiter error %v, want errDumpPanicked", err)
	}
	// With the only slot leaked, this would fail with errDumpBusy.
	_, release, err := s.dumpCoalesced(context.Background(), p)
	release()
	if err != nil {
		t.Fatalf("dump after panic: %v", err)
	}
}
