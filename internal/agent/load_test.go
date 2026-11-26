package agent

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// RunLoadSampler records a load within a couple of intervals and stops.
func TestRunLoadSampler(t *testing.T) {
	dir := t.TempDir()
	writeProc(t, dir, "cpu  100 0 100 800 0 0 0 0 0 0\n")
	s := newServer(t, t.TempDir())
	s.ProcDir = dir
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s.RunLoadSampler(5*time.Millisecond, stop)
	}()
	// Keep the counters moving so every sample has elapsed time.
	deadline := time.Now().Add(5 * time.Second)
	for i := uint64(1); s.currentLoad() == nil; i++ {
		if time.Now().After(deadline) {
			t.Fatal("no load sampled")
		}
		writeProc(t, dir, fmt.Sprintf("cpu  %d 0 100 %d 0 0 0 0 0 0\n", 100+i, 800+i))
		time.Sleep(time.Millisecond)
	}
	close(stop)
	<-done
}

func writeProc(t *testing.T, dir, stat string) {
	t.Helper()
	mustFile(t, filepath.Join(dir, "stat"), []byte(stat))
	mustFile(t, filepath.Join(dir, "meminfo"), []byte(
		"MemTotal:       16000000 kB\nMemFree:         1000000 kB\nMemAvailable:    4000000 kB\n"))
}

func TestReadCPUAndMemory(t *testing.T) {
	dir := t.TempDir()
	// user nice system idle iowait irq softirq steal guest guest_nice
	writeProc(t, dir, "cpu  100 0 100 700 100 0 0 0 50 0\ncpu0 1 2 3 4 5 6 7 8 9 10\n")
	got, err := readCPU(dir)
	if err != nil {
		t.Fatal(err)
	}
	// guest is inside user, so it is not added again; idle and iowait are not busy.
	if got != (cpuTimes{busy: 200, total: 1000}) {
		t.Fatalf("readCPU = %+v, want busy 200 of 1000", got)
	}
	mem, err := readMemory(dir)
	if err != nil || mem != 0.75 {
		t.Fatalf("readMemory = %v, %v; want 0.75", mem, err)
	}
	if u, ok := cpuUtil(cpuTimes{busy: 200, total: 1000}, cpuTimes{busy: 350, total: 1200}); !ok || u != 0.75 {
		t.Fatalf("cpuUtil = %v, %v; want 0.75", u, ok)
	}
	if _, ok := cpuUtil(got, got); ok {
		t.Fatal("cpuUtil over no elapsed time reported a value")
	}
}

func TestLoadSamplerAndMissingReply(t *testing.T) {
	dir := t.TempDir()
	s := newServer(t, t.TempDir())
	s.ProcDir = dir
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// Before any sample the reply carries no load, so an extender treats the
	// node as unloaded, as it does an older agent.
	const body = `{"paths": [{"path": "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-x", "narSize": 1}]}`
	code, out, raw := postMissing(t, srv.URL, body)
	if code != 200 {
		t.Fatalf("missing_bytes: %d %s", code, raw)
	}
	if _, ok := out["cpu_util"]; ok {
		t.Fatalf("reply carries cpu_util before any sample: %v", out)
	}

	writeProc(t, dir, "cpu  100 0 100 800 0 0 0 0 0 0\n")
	prev, err := readCPU(dir)
	if err != nil {
		t.Fatal(err)
	}
	// 300 more busy jiffies out of 400: 75% busy since the first reading.
	writeProc(t, dir, "cpu  400 0 100 900 0 0 0 0 0 0\n")
	if _, err := s.sampleLoad(dir, prev); err != nil {
		t.Fatal(err)
	}

	if code, out, raw = postMissing(t, srv.URL, body); code != 200 {
		t.Fatalf("missing_bytes: %d %s", code, raw)
	}
	var cpu, mem float64
	if err := json.Unmarshal(out["cpu_util"], &cpu); err != nil {
		t.Fatalf("cpu_util in %v: %v", out, err)
	}
	if err := json.Unmarshal(out["mem_util"], &mem); err != nil {
		t.Fatalf("mem_util in %v: %v", out, err)
	}
	if math.Abs(cpu-0.75) > 1e-9 || mem != 0.75 {
		t.Fatalf("reply load cpu=%v mem=%v, want 0.75 and 0.75", cpu, mem)
	}
}

func TestReadMemoryNeedsBothFields(t *testing.T) {
	dir := t.TempDir()
	mustFile(t, filepath.Join(dir, "meminfo"), []byte("MemTotal: 100 kB\n"))
	if _, err := readMemory(dir); err == nil {
		t.Fatal("readMemory accepted a meminfo without MemAvailable")
	}
	if err := os.Remove(filepath.Join(dir, "meminfo")); err != nil {
		t.Fatal(err)
	}
	if _, err := readMemory(dir); err == nil {
		t.Fatal("readMemory accepted a missing meminfo")
	}
}
