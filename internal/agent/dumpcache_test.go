package agent

import (
	"io"
	"os"
	"path/filepath"
	"sync"
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
		res, release, err := s.dumpCoalesced(a)
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
	_, releaseB, err := s.dumpCoalesced(b)
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
	_, releaseB, err = s.dumpCoalesced(b)
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
		res, release, err := s.dumpCoalesced(paths[name])
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
	s.DumpFunc = func(path string, w io.Writer) error {
		_, err := w.Write([]byte("too-big"))
		return err
	}
	res, release, err := s.dumpCoalesced(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.file); err != nil {
		t.Fatalf("oversized dump not available while held: %v", err)
	}
	release()
	if _, err := os.Stat(res.file); !os.IsNotExist(err) {
		t.Fatalf("oversized dump kept after release (stat err: %v)", err)
	}
}
