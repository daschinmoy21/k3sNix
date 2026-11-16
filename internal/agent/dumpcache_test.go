package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSlowDumpSurvivesSweep(t *testing.T) {
	// A dump that takes longer than DumpTTL must still be on disk for the
	// leader and the waiters when another request sweeps the cache.
	dir := t.TempDir()
	a := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pkg")
	b := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-pkg")
	mustDir(t, a)
	mustDir(t, b)
	s := newServer(t, dir)
	s.DumpTTL = 20 * time.Millisecond
	started := make(chan struct{})
	finish := make(chan struct{})
	s.DumpFunc = func(path string, w io.Writer) error {
		if path == a {
			close(started)
			<-finish
		}
		_, err := w.Write([]byte("payload:" + path))
		return err
	}

	type held struct {
		res     dumpResult
		release func()
		err     error
	}
	var wg sync.WaitGroup
	results := make([]held, 2)
	get := func(i int) {
		defer wg.Done()
		res, release, err := s.dumpCoalesced(context.Background(), a)
		results[i] = held{res, release, err}
	}
	wg.Add(1)
	go get(0) // leader
	<-started
	wg.Add(1)
	go get(1) // waiter
	time.Sleep(3 * s.DumpTTL)
	close(finish)
	wg.Wait()

	// Another path's request sweeps the cache while a is still held.
	_, releaseB, err := s.dumpCoalesced(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	releaseB()

	for i, h := range results {
		if h.err != nil {
			t.Fatalf("caller %d: %v", i, h.err)
		}
		f, err := os.Open(h.res.file)
		if err != nil {
			t.Fatalf("caller %d: held dump was evicted: %v", i, err)
		}
		f.Close()
		h.release()
	}

	// Once released and idle past the TTL, the entry goes.
	time.Sleep(3 * s.DumpTTL)
	_, releaseB, err = s.dumpCoalesced(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	releaseB()
	if _, err := os.Stat(results[0].res.file); !os.IsNotExist(err) {
		t.Fatalf("released dump still on disk after TTL (stat err: %v)", err)
	}
}

func TestDumpCacheByteCap(t *testing.T) {
	dir := t.TempDir()
	paths := map[string]string{}
	for _, name := range []string{"a", "b", "c"} {
		p := filepath.Join(dir, name+"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pkg")
		mustDir(t, p)
		paths[name] = p
	}
	s := newServer(t, dir)
	s.MaxDumpBytes = 10
	s.DumpFunc = func(path string, w io.Writer) error {
		_, err := w.Write([]byte("12345")) // two dumps fit the cap, three do not
		return err
	}
	dump := func(name string) (dumpResult, func()) {
		t.Helper()
		res, release, err := s.dumpCoalesced(context.Background(), paths[name])
		if err != nil {
			t.Fatal(err)
		}
		return res, release
	}
	exists := func(f string) bool {
		_, err := os.Stat(f)
		return err == nil
	}

	resA, release := dump("a")
	release()
	resB, releaseB := dump("b") // held for the rest of the test
	resC, release := dump("c")
	release()

	// Releasing c put the total at 15 > 10: a is the least recently used
	// idle entry and goes; b is held; c fits once a is gone.
	if exists(resA.file) {
		t.Error("least recently used dump a still on disk")
	}
	if !exists(resB.file) {
		t.Error("held dump b was evicted")
	}
	if !exists(resC.file) {
		t.Error("dump c evicted although the cache fits it")
	}
	s.mu.Lock()
	total, n := s.dumpBytes, len(s.dumps)
	s.mu.Unlock()
	if total != 10 || n != 2 {
		t.Fatalf("cache holds %d bytes in %d entries, want 10 in 2", total, n)
	}
	releaseB()
}

func TestDumpLargerThanCapIsServedThenDropped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pkg")
	mustDir(t, p)
	s := newServer(t, dir)
	s.MaxDumpBytes = 4
	s.DumpTTL = 20 * time.Millisecond
	var dumps atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		dumps.Add(1)
		_, err := w.Write([]byte("too-big"))
		return err
	}
	res, release, err := s.dumpCoalesced(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.file); err != nil {
		t.Fatalf("oversized dump not available while held: %v", err)
	}
	release()

	// An oversized dump is kept briefly after release so the following
	// NAR request can reuse the dump that narinfo produced.
	if _, err := os.Stat(res.file); err != nil {
		t.Fatalf("oversized dump was evicted immediately upon release: %v", err)
	}
	res2, release2, err := s.dumpCoalesced(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if got := dumps.Load(); got != 1 {
		t.Fatalf("oversized dump was dumped %d times, want 1 (reused from cache)", got)
	}
	if res2.file != res.file {
		t.Fatalf("reused dump file %s != %s", res2.file, res.file)
	}
	release2()

	// Once idle past the TTL, the oversized entry is evicted.
	time.Sleep(3 * s.DumpTTL)
	s.mu.Lock()
	s.evictLocked(time.Now())
	s.mu.Unlock()
	if _, err := os.Stat(res.file); !os.IsNotExist(err) {
		t.Fatalf("oversized dump kept after TTL (stat err: %v)", err)
	}
}

func TestMaxConcurrentDumpsLimit(t *testing.T) {
	dir := t.TempDir()
	const maxConcurrent = 3
	const totalRequests = 10

	paths := make([]string, totalRequests)
	for i := 0; i < totalRequests; i++ {
		p := filepath.Join(dir, fmt.Sprintf("%032d-pkg", i))
		mustDir(t, p)
		paths[i] = p
	}

	s := newServer(t, dir)
	s.MaxConcurrentDumps = maxConcurrent

	var (
		activeDumps atomic.Int32
		maxActive   atomic.Int32
		totalDumps  atomic.Int32
		gate        = make(chan struct{})
		reachedMax  = make(chan struct{})
		onceReached sync.Once
	)

	s.DumpFunc = func(path string, w io.Writer) error {
		totalDumps.Add(1)
		cur := activeDumps.Add(1)
		defer activeDumps.Add(-1)

		for {
			old := maxActive.Load()
			if cur <= old || maxActive.CompareAndSwap(old, cur) {
				break
			}
		}

		if cur == maxConcurrent {
			onceReached.Do(func() {
				close(reachedMax)
			})
		}

		<-gate
		_, err := w.Write([]byte("dump-payload"))
		return err
	}

	var wg sync.WaitGroup
	for _, p := range paths {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			res, release, err := s.dumpCoalesced(context.Background(), path)
			if err != nil {
				t.Errorf("dumpCoalesced failed: %v", err)
				return
			}
			release()
			if res.file == "" {
				t.Errorf("dumpResult has empty file")
			}
		}(p)
	}

	// Wait until concurrency limit is reached.
	select {
	case <-reachedMax:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for concurrent dumps to reach limit")
	}

	// Sleep briefly so other goroutines have a chance to run if limit were missing.
	time.Sleep(100 * time.Millisecond)

	// Verify currently active dumps is exactly the cap.
	if got := activeDumps.Load(); got != maxConcurrent {
		t.Fatalf("active dumps = %d, want exactly %d", got, maxConcurrent)
	}

	// Release the gate and allow all dumps to finish.
	close(gate)
	wg.Wait()

	if got := maxActive.Load(); got != maxConcurrent {
		t.Fatalf("max concurrent dumps observed was %d, want %d", got, maxConcurrent)
	}
	if got := totalDumps.Load(); got != totalRequests {
		t.Fatalf("total dumps executed %d, want %d", got, totalRequests)
	}
}

func TestSamePathCoalescingDoesNotConsumeMultipleSlots(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pkg")
	mustDir(t, p)

	s := newServer(t, dir)
	// With a limit of 1, if coalesced requests attempted to acquire slots,
	// they would block or deadlock.
	s.MaxConcurrentDumps = 1

	var (
		dumpsStarted atomic.Int32
		dumpGate     = make(chan struct{})
		started      = make(chan struct{})
		onceStarted  sync.Once
	)

	s.DumpFunc = func(path string, w io.Writer) error {
		dumpsStarted.Add(1)
		onceStarted.Do(func() {
			close(started)
		})
		<-dumpGate
		_, err := w.Write([]byte("coalesced-data"))
		return err
	}

	const callers = 5
	var wg sync.WaitGroup
	results := make([]dumpResult, callers)

	// Start leader
	wg.Add(1)
	go func() {
		defer wg.Done()
		res, release, err := s.dumpCoalesced(context.Background(), p)
		if err != nil {
			t.Errorf("leader dump failed: %v", err)
			return
		}
		release()
		results[0] = res
	}()

	// Wait until leader has started dumping
	<-started

	// Start multiple concurrent waiters for the exact same path
	for i := 1; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			res, release, err := s.dumpCoalesced(context.Background(), p)
			if err != nil {
				t.Errorf("waiter %d dump failed: %v", idx, err)
				return
			}
			release()
			results[idx] = res
		}(i)
	}

	// Poll under s.mu until all callers have attached (refs == callers).
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		c, ok := s.dumps[p]
		refs := 0
		if ok {
			refs = c.refs
		}
		s.mu.Unlock()
		if refs == callers {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d callers to attach, got refs=%d", callers, refs)
		}
		time.Sleep(1 * time.Millisecond)
	}

	// Verify while gate is closed that only one dump slot is in use.
	s.mu.Lock()
	semLen := len(s.dumpSem)
	s.mu.Unlock()
	if semLen != 1 {
		t.Fatalf("slots in use = %d, want 1", semLen)
	}

	// Finish the leader's dump
	close(dumpGate)
	wg.Wait()

	if got := dumpsStarted.Load(); got != 1 {
		t.Fatalf("DumpFunc called %d times for same path, want 1", got)
	}

	for i := 1; i < callers; i++ {
		if results[i].file != results[0].file {
			t.Fatalf("caller %d got file %s, want %s", i, results[i].file, results[0].file)
		}
		if results[i].hash != results[0].hash {
			t.Fatalf("caller %d got hash %s, want %s", i, results[i].hash, results[0].hash)
		}
	}
}

func TestContextExpirationWhileWaitingReturns503(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-p1")
	p2 := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-p2")
	mustDir(t, p1)
	mustDir(t, p2)

	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1

	p1Started := make(chan struct{})
	p1Block := make(chan struct{})

	s.DumpFunc = func(path string, w io.Writer) error {
		if path == p1 {
			close(p1Started)
			<-p1Block
		}
		_, err := w.Write([]byte("dump-" + path))
		return err
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	var wg sync.WaitGroup
	var p1Once sync.Once
	defer func() {
		p1Once.Do(func() { close(p1Block) })
		wg.Wait()
	}()

	// Start request 1 for p1 (occupies the only slot)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get(srv.URL + "/nar/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.nar")
		if err != nil {
			t.Errorf("request 1 failed: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("request 1 status %d, want 200", resp.StatusCode)
		}
	}()

	<-p1Started

	// Send request 2 for p2 with a short context timeout
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nar/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.nar", nil).WithContext(ctx)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("request 2 status = %d, want 503 Service Unavailable", rec.Code)
	}

	// Also verify .narinfo returns 503 on context expiration
	ctxInfo, cancelInfo := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelInfo()

	recInfo := httptest.NewRecorder()
	reqInfo := httptest.NewRequest(http.MethodGet, "/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.narinfo", nil).WithContext(ctxInfo)
	s.Handler().ServeHTTP(recInfo, reqInfo)

	if recInfo.Code != http.StatusServiceUnavailable {
		t.Fatalf("narinfo status = %d, want 503 Service Unavailable", recInfo.Code)
	}

	// Unblock request 1 and finish
	p1Once.Do(func() { close(p1Block) })
	wg.Wait()
}

func TestOversizedDumpReusedByNarAfterNarinfo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pkg")
	mustDir(t, p)

	s := newServer(t, dir)
	// Dump size (20) exceeds MaxDumpBytes (10)
	s.MaxDumpBytes = 10
	s.DumpTTL = 200 * time.Millisecond

	var dumpCount atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		dumpCount.Add(1)
		_, err := w.Write([]byte("01234567890123456789")) // 20 bytes
		return err
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 1. Client requests narinfo
	resp1, err := http.Get(srv.URL + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("narinfo status %d, want 200", resp1.StatusCode)
	}

	// 2. Client requests NAR immediately after
	resp2, err := http.Get(srv.URL + "/nar/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.nar")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("nar status %d, want 200", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if len(body) != 20 {
		t.Fatalf("nar body length = %d, want 20", len(body))
	}

	// Assert DumpFunc was executed ONLY ONCE across both requests
	if got := dumpCount.Load(); got != 1 {
		t.Fatalf("DumpFunc executed %d times, want 1 (oversized dump was not reused)", got)
	}
}

func TestSmallPathNotEvictedByOversizedDump(t *testing.T) {
	dir := t.TempDir()
	pLarge := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-large")
	pSmall := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-small")
	mustDir(t, pLarge)
	mustDir(t, pSmall)

	s := newServer(t, dir)
	// MaxDumpBytes is 10 bytes.
	// pLarge is 20 bytes (oversized).
	// pSmall is 4 bytes (small).
	s.MaxDumpBytes = 10
	s.DumpTTL = 500 * time.Millisecond

	var largeDumps, smallDumps atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		if path == pLarge {
			largeDumps.Add(1)
			_, err := w.Write([]byte("01234567890123456789")) // 20 bytes
			return err
		}
		smallDumps.Add(1)
		_, err := w.Write([]byte("1234")) // 4 bytes
		return err
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 1. Client requests narinfo for small path
	resp, err := http.Get(srv.URL + "/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 2. Client requests narinfo for oversized path
	resp, err = http.Get(srv.URL + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 3. Client requests NAR for small path - must reuse the dump from step 1!
	resp, err = http.Get(srv.URL + "/nar/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.nar")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 4. Client requests NAR for oversized path - must reuse the dump from step 2!
	resp, err = http.Get(srv.URL + "/nar/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.nar")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Assert BOTH were dumped exactly ONCE
	if got := smallDumps.Load(); got != 1 {
		t.Fatalf("small path dumped %d times, want 1 (small dump was evicted prematurely)", got)
	}
	if got := largeDumps.Load(); got != 1 {
		t.Fatalf("oversized path dumped %d times, want 1", got)
	}
}

func TestOversizedDumpsDoNotAccumulateOnDisk(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-p1")
	p2 := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-p2")
	mustDir(t, p1)
	mustDir(t, p2)

	s := newServer(t, dir)
	s.MaxDumpBytes = 10

	s.DumpFunc = func(path string, w io.Writer) error {
		_, err := w.Write([]byte("01234567890123456789")) // 20 bytes (oversized)
		return err
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 1. Dump oversized p1 via narinfo
	resp1, err := http.Get(srv.URL + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	resp1.Body.Close()

	// p1 is now the single idle oversized dump in cache
	s.mu.Lock()
	p1Call, p1Present := s.dumps[p1]
	p1File := ""
	if p1Present {
		p1File = p1Call.res.file
	}
	s.mu.Unlock()
	if !p1Present || p1File == "" {
		t.Fatalf("p1 should be in cache after narinfo")
	}
	if _, err := os.Stat(p1File); err != nil {
		t.Fatalf("p1 file should exist on disk: %v", err)
	}

	// 2. Dump a second oversized path p2 via narinfo
	resp2, err := http.Get(srv.URL + "/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	// Assert p1 was dropped and unlinked, so oversized dumps do not accumulate
	s.mu.Lock()
	_, p1StillInCache := s.dumps[p1]
	p2Call, p2Present := s.dumps[p2]
	p2File := ""
	if p2Present {
		p2File = p2Call.res.file
	}
	s.mu.Unlock()

	if p1StillInCache {
		t.Fatalf("p1 should have been evicted when new oversized p2 arrived")
	}
	if _, err := os.Stat(p1File); !os.IsNotExist(err) {
		t.Fatalf("p1 file should have been removed from disk, err=%v", err)
	}
	if !p2Present || p2File == "" {
		t.Fatalf("p2 should be in cache after narinfo")
	}
	if _, err := os.Stat(p2File); err != nil {
		t.Fatalf("p2 file should exist on disk: %v", err)
	}
}

func TestPanicInDumpFuncDoesNotPoisonPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-panicpkg")
	mustDir(t, p)

	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1

	var calls atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		if calls.Add(1) == 1 {
			panic("simulated dump explosion")
		}
		_, err := w.Write([]byte("healthy dump"))
		return err
	}

	// First call should return an error, not panic
	ctx1, cancel1 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel1()
	_, release1, err1 := s.dumpCoalesced(ctx1, p)
	if err1 == nil {
		release1()
		t.Fatalf("first call should have failed with error, got nil")
	}
	release1()

	// Semaphore slot must be freed immediately (len == 0)
	s.mu.Lock()
	semLen := len(s.dumpSem)
	s.mu.Unlock()
	if semLen != 0 {
		t.Fatalf("semaphore slot leaked: len = %d, want 0", semLen)
	}

	// Second call must succeed within 2 seconds
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	res2, release2, err2 := s.dumpCoalesced(ctx2, p)
	if err2 != nil {
		t.Fatalf("second call should have succeeded, got error: %v", err2)
	}
	defer release2()
	if res2.size != int64(len("healthy dump")) {
		t.Fatalf("second call size = %d, want %d", res2.size, len("healthy dump"))
	}
}

func TestWaiterTakesOverWhenLeaderContextExpires(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-pathA")
	pathB := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-pathB")
	mustDir(t, pathA)
	mustDir(t, pathB)

	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1

	gateA := make(chan struct{})
	startedA := make(chan struct{})
	var dumpsB atomic.Int32

	s.DumpFunc = func(path string, w io.Writer) error {
		if path == pathA {
			close(startedA)
			<-gateA
			_, err := w.Write([]byte("dumpA"))
			return err
		}
		if path == pathB {
			dumpsB.Add(1)
			_, err := w.Write([]byte("dumpB"))
			return err
		}
		return nil
	}

	// 1. Path A holds the only slot
	var wgA sync.WaitGroup
	wgA.Add(1)
	go func() {
		defer wgA.Done()
		res, release, err := s.dumpCoalesced(context.Background(), pathA)
		if err == nil {
			release()
		}
		_ = res
	}()
	<-startedA

	// 2. Start leader for path B with a short timeout (30ms). It will wait for a slot.
	ctxLeader, cancelLeader := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelLeader()

	var wgLeader sync.WaitGroup
	var errLeader error
	wgLeader.Add(1)
	go func() {
		defer wgLeader.Done()
		_, release, err := s.dumpCoalesced(ctxLeader, pathB)
		if err == nil {
			release()
		}
		errLeader = err
	}()

	// 3. Poll until s.dumps[pathB].refs == 1 under s.mu
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		c, ok := s.dumps[pathB]
		refs := 0
		if ok {
			refs = c.refs
		}
		s.mu.Unlock()
		if refs == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for leader B to register")
		}
		time.Sleep(1 * time.Millisecond)
	}

	// Start a waiter for B with context.Background()
	ctxWaiter, cancelWaiter := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWaiter()

	var wgWaiter sync.WaitGroup
	var resWaiter dumpResult
	var errWaiter error
	wgWaiter.Add(1)
	go func() {
		defer wgWaiter.Done()
		res, release, err := s.dumpCoalesced(ctxWaiter, pathB)
		if err == nil {
			release()
		}
		resWaiter = res
		errWaiter = err
	}()

	// Poll until refs == 2
	for {
		s.mu.Lock()
		c, ok := s.dumps[pathB]
		refs := 0
		if ok {
			refs = c.refs
		}
		s.mu.Unlock()
		if refs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for waiter B to attach")
		}
		time.Sleep(1 * time.Millisecond)
	}

	// 4. Wait for leader context to expire
	wgLeader.Wait()
	if !errors.Is(errLeader, context.DeadlineExceeded) && !errors.Is(errLeader, context.Canceled) {
		t.Fatalf("leader expected context deadline exceeded, got: %v", errLeader)
	}

	// 5. Open gate for A, releasing the slot
	close(gateA)
	wgA.Wait()

	// Waiter must take over as leader, acquire the slot, and succeed
	wgWaiter.Wait()
	if errWaiter != nil {
		t.Fatalf("waiter failed after leader gave up: %v", errWaiter)
	}
	if resWaiter.size != int64(len("dumpB")) {
		t.Fatalf("waiter got size %d, want %d", resWaiter.size, len("dumpB"))
	}
	if got := dumpsB.Load(); got != 1 {
		t.Fatalf("DumpFunc for path B executed %d times, want 1", got)
	}
}

func TestCancelledContextDoesNotStartDump(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-cancelled")
	mustDir(t, p)

	s := newServer(t, dir)
	var dumps atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		dumps.Add(1)
		_, err := w.Write([]byte("dump-cancelled"))
		return err
	}

	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel before calling dumpCoalesced

		_, release, err := s.dumpCoalesced(ctx, p)
		if release != nil {
			release()
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: expected context.Canceled, got: %v", i, err)
		}
	}

	if got := dumps.Load(); got != 0 {
		t.Fatalf("DumpFunc called %d times for cancelled context, want 0", got)
	}
}

func TestJanitorEvictsExpiredDumps(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-janitor")
	mustDir(t, p)

	s := newServer(t, dir)
	s.DumpTTL = 20 * time.Millisecond
	s.DumpFunc = func(path string, w io.Writer) error {
		_, err := w.Write([]byte("janitor-data"))
		return err
	}

	res, release, err := s.dumpCoalesced(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.file); err != nil {
		t.Fatalf("dump file should exist on disk: %v", err)
	}
	release()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartJanitor(ctx, 10*time.Millisecond)

	// With no other requests, wait and assert file is gone within a second.
	deadline := time.Now().Add(1 * time.Second)
	removed := false
	for time.Now().Before(deadline) {
		if _, err := os.Stat(res.file); os.IsNotExist(err) {
			removed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !removed {
		t.Fatalf("janitor failed to remove expired dump %s within 1s", res.file)
	}
}

func TestCleanStaleDumps(t *testing.T) {
	dir := t.TempDir()

	f1 := filepath.Join(dir, "k3snix-nar-123456")
	f2 := filepath.Join(dir, "k3snix-nar-abcdef")
	f3 := filepath.Join(dir, "k3snix-nar-temp99")
	unrelated := filepath.Join(dir, "unrelated.txt")

	for _, f := range []string{f1, f2, f3, unrelated} {
		if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed := CleanStaleDumps(dir)
	if removed != 3 {
		t.Fatalf("CleanStaleDumps removed %d files, want 3", removed)
	}

	for _, f := range []string{f1, f2, f3} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("file %s should have been deleted", f)
		}
	}

	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated file should not have been deleted: %v", err)
	}
}

func TestDumpMetricsSlotsAndTimeout(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-p1")
	p2 := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-p2")
	mustDir(t, p1)
	mustDir(t, p2)

	s := newServer(t, dir)
	s.MaxConcurrentDumps = 1

	gate := make(chan struct{})
	started := make(chan struct{})

	s.DumpFunc = func(path string, w io.Writer) error {
		if path == p1 {
			close(started)
			<-gate
		}
		_, err := w.Write([]byte("dump-data"))
		return err
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	var wg sync.WaitGroup
	var onceGate sync.Once
	defer func() {
		onceGate.Do(func() { close(gate) })
		wg.Wait()
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		res, release, err := s.dumpCoalesced(context.Background(), p1)
		if err == nil {
			release()
		}
		_ = res
	}()

	<-started

	getMetrics := func() string {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	m1 := getMetrics()
	if !strings.Contains(m1, "k3snix_dump_slots_in_use 1\n") {
		t.Fatalf("expected k3snix_dump_slots_in_use 1, got:\n%s", m1)
	}
	if !strings.Contains(m1, "k3snix_dump_slot_timeouts 0\n") {
		t.Fatalf("expected k3snix_dump_slot_timeouts 0, got:\n%s", m1)
	}

	// Trigger timeout on p2
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, release2, err := s.dumpCoalesced(ctx, p2)
	if release2 != nil {
		release2()
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context timeout, got %v", err)
	}

	m2 := getMetrics()
	if !strings.Contains(m2, "k3snix_dump_slot_timeouts 1\n") {
		t.Fatalf("expected k3snix_dump_slot_timeouts 1, got:\n%s", m2)
	}

	onceGate.Do(func() { close(gate) })
	wg.Wait()

	m3 := getMetrics()
	if !strings.Contains(m3, "k3snix_dump_slots_in_use 0\n") {
		t.Fatalf("expected k3snix_dump_slots_in_use 0, got:\n%s", m3)
	}
}

func TestAlternatingOversizedPathsDumpTwice(t *testing.T) {
	dir := t.TempDir()
	p1 := filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-p1")
	p2 := filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-p2")
	mustDir(t, p1)
	mustDir(t, p2)

	s := newServer(t, dir)
	s.MaxDumpBytes = 10

	var p1Dumps, p2Dumps atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		if path == p1 {
			p1Dumps.Add(1)
		} else if path == p2 {
			p2Dumps.Add(1)
		}
		_, err := w.Write([]byte("01234567890123456789")) // 20 bytes (> 10)
		return err
	}

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 1. Client requests p1.narinfo (dumps p1, count 1)
	resp, err := http.Get(srv.URL + "/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 2. Client requests p2.narinfo (dumps p2, count 1; evicts p1)
	resp, err = http.Get(srv.URL + "/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 3. Client requests p1.nar (p1 was evicted by p2, so it re-dumps, count 2)
	resp, err = http.Get(srv.URL + "/nar/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.nar")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got := p1Dumps.Load(); got != 2 {
		t.Fatalf("p1 dumped %d times, want 2 (alternating oversized paths evict each other)", got)
	}
	if got := p2Dumps.Load(); got != 1 {
		t.Fatalf("p2 dumped %d times, want 1", got)
	}
}
