package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// NodeLoad is the node's utilisation the agent reports in missing_bytes
// replies, so the extender can weigh the cache bonus by load without a
// Kubernetes client. Both values are fractions in [0, 1].
type NodeLoad struct {
	CPU    float64 // busy share of all CPUs over the last sample interval
	Memory float64 // 1 - MemAvailable/MemTotal
}

// SetLoad records the node's current load. RunLoadSampler calls it; tests
// call it directly.
func (s *Server) SetLoad(l NodeLoad) {
	s.load.Store(&l)
}

// currentLoad returns the last recorded load, or nil before the first
// sample, in which case replies carry no load.
func (s *Server) currentLoad() *NodeLoad {
	return s.load.Load()
}

// RunLoadSampler samples CPU and memory use from ProcDir every interval
// until stop is closed. CPU use needs two readings, so the first load is
// recorded one interval after the start. In a container /proc/stat and
// /proc/meminfo describe the whole node, which is what is wanted here.
func (s *Server) RunLoadSampler(interval time.Duration, stop <-chan struct{}) {
	proc := s.ProcDir
	if proc == "" {
		proc = "/proc"
	}
	prev, err := readCPU(proc)
	if err != nil {
		s.logError("load", proc, err)
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		cur, err := s.sampleLoad(proc, prev)
		if err != nil {
			s.logError("load", proc, err)
			continue
		}
		prev = cur
	}
}

// sampleLoad reads the CPU times and memory use now, records the load since
// prev, and returns the CPU times for the next sample.
func (s *Server) sampleLoad(proc string, prev cpuTimes) (cpuTimes, error) {
	cur, err := readCPU(proc)
	if err != nil {
		return prev, err
	}
	mem, err := readMemory(proc)
	if err != nil {
		return prev, err
	}
	if cpu, ok := cpuUtil(prev, cur); ok {
		s.SetLoad(NodeLoad{CPU: cpu, Memory: mem})
	}
	return cur, nil
}

// cpuTimes are the aggregate jiffies of the "cpu" line of /proc/stat.
type cpuTimes struct {
	busy, total uint64
}

func readCPU(proc string) (cpuTimes, error) {
	f, err := os.Open(filepath.Join(proc, "stat"))
	if err != nil {
		return cpuTimes{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		// user nice system idle iowait irq softirq steal guest guest_nice.
		// guest and guest_nice are already counted in user and nice.
		var t cpuTimes
		for i, v := range fields[1:] {
			if i >= 8 {
				break
			}
			n, err := strconv.ParseUint(v, 10, 64)
			if err != nil {
				return cpuTimes{}, fmt.Errorf("stat: %q: %w", sc.Text(), err)
			}
			t.total += n
			if i != 3 && i != 4 { // idle, iowait
				t.busy += n
			}
		}
		return t, nil
	}
	if err := sc.Err(); err != nil {
		return cpuTimes{}, err
	}
	return cpuTimes{}, errors.New("stat: no cpu line")
}

// cpuUtil is the busy share between two readings.
func cpuUtil(prev, cur cpuTimes) (float64, bool) {
	if cur.total <= prev.total || cur.busy < prev.busy {
		return 0, false
	}
	return float64(cur.busy-prev.busy) / float64(cur.total-prev.total), true
}

func readMemory(proc string) (float64, error) {
	f, err := os.Open(filepath.Join(proc, "meminfo"))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var total, avail uint64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		n, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total, haveTotal = n, true
		case "MemAvailable:":
			avail, haveAvail = n, true
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if !haveTotal || !haveAvail || total == 0 || avail > total {
		return 0, errors.New("meminfo: no MemTotal and MemAvailable")
	}
	return 1 - float64(avail)/float64(total), nil
}
