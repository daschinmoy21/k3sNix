package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"k3snix/internal/closure"
	"k3snix/internal/extender"
)

func main() {
	var (
		listen   = flag.String("listen", ":9870", "http listen address")
		catalog  = flag.String("catalog", "", "directory of closure JSON files")
		agentURL = flag.String("agent-url", "http://{ip}:9860", "node agent base URL; {ip} is the node's InternalIP, {node} its name")
		timeout  = flag.Duration("agent-timeout", 500*time.Millisecond, "bound on one agent missing_bytes call; a slower node scores 0")
		scale    = flag.Uint64("scale-bytes", extender.DefaultScaleBytes, "closure size at which the score is the held share; smaller closures score near 10 everywhere (0: held share only)")
		highCPU  = flag.Float64("high-cpu", extender.DefaultHighCPU, "CPU use at which a node's score is scaled by -loaded-factor (LRScheduler h_CPU)")
		highStd  = flag.Float64("high-spread", extender.DefaultHighSpread, "std dev of CPU use across nodes at which every score is scaled by -loaded-factor (LRScheduler h_STD)")
		loaded   = flag.Float64("loaded-factor", extender.DefaultLoadedFactor, "score multiplier for a loaded node or an unbalanced cluster")
	)
	flag.Parse()
	if *catalog == "" {
		log.Fatal("-catalog is required")
	}
	cat, err := closure.LoadCatalog(*catalog)
	if err != nil {
		log.Fatalf("catalog %s: %v", *catalog, err)
	}
	s := extender.New(cat)
	s.AgentURL = *agentURL
	s.AgentTimeout = *timeout
	s.ScaleBytes = *scale
	s.HighCPU, s.HighSpread, s.LoadedFactor = *highCPU, *highStd, *loaded
	srv := &http.Server{
		Addr:              *listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("k3snix-extender listening on %s, catalog %s, agents %s", *listen, *catalog, *agentURL)
	log.Fatal(srv.ListenAndServe())
}
