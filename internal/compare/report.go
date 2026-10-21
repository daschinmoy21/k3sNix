package compare

import (
	"fmt"
	"io"
	"strings"
)

// Node is a simulated node: the workloads it already holds.
type Node struct {
	Name  string   `json:"name"`
	Holds []string `json:"holds"`
}

// Nodes are the three nodes of the Phase 1 demo: one on an older version
// of the churn workload, one running hello (which shares glibc with
// coreutils), and an empty one.
var Nodes = []Node{
	{Name: "node-churn-v1", Holds: []string{"churn-v1"}},
	{Name: "node-hello", Holds: []string{"share-a"}},
	{Name: "node-empty", Holds: nil},
}

// Steps are the version updates of the churn ladder.
var Steps = [][2]string{
	{"churn-v1", "churn-v2"},
	{"churn-v2", "churn-v3"},
	{"churn-v3", "churn-v4"},
	{"churn-v1", "churn-v4"},
}

// Bytes is a missing/total pair.
type Bytes struct {
	Missing uint64 `json:"missing"`
	Total   uint64 `json:"total"`
}

// Cell is one (node, target, delivery) measurement.
type Cell struct {
	Node     string `json:"node"`
	Target   string `json:"target"`
	Delivery string `json:"delivery"`
	Units    Bytes  `json:"units"` // counts, not bytes
	Raw      Bytes  `json:"raw"`
	Gzip     Bytes  `json:"gzip"`
}

// Step is one churn update in one delivery.
type Step struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Delivery string `json:"delivery"`
	Raw      Bytes  `json:"raw"`
	Gzip     Bytes  `json:"gzip"`
}

// Placement is which nodes a scorer prefers for a target, and the raw bytes
// the chosen node then transfers under that scorer's delivery design, whole
// images for OCI. A tie lists every tied node; the transfer is then a range
// over them.
type Placement struct {
	Target   string   `json:"target"`
	Arm      string   `json:"arm"`
	Delivery string   `json:"delivery"`
	Picked   []string `json:"picked"`
	// Scores is each node's score under the arm: a byte fraction for
	// paths and layers, ImageLocality's 0..100 for image, 0 for none.
	Scores     map[string]float64 `json:"scores"`
	ImageSizes map[string]uint64  `json:"imageSizes,omitempty"`
	// TransferByNode includes every node, even those outside Picked.
	// Live schedulers can choose any feasible node; validate that actual pull.
	TransferByNode map[string]uint64 `json:"transferByNode"`
	TransferMin    uint64            `json:"transferMin"`
	TransferMax    uint64            `json:"transferMax"`
}

// Arm is a scheduler paired with a delivery design.
type Arm struct {
	Name     string `json:"name"`
	Delivery string `json:"delivery"`
	// Signal is what the scheduler can see: "paths" (NAR bytes on the
	// node), "layers" (layer bytes on the node), "image" (ImageLocality:
	// the size of the exact image if the node holds it) or "none".
	Signal string `json:"signal"`
}

// Arms are the evaluation arms that can be scored from bytes alone.
var Arms = []Arm{
	{Name: "k3snix", Delivery: Nix, Signal: "paths"},
	{Name: "default-hoststore", Delivery: Nix, Signal: "none"},
	{Name: "lrscheduler (fine)", Delivery: OCIFine, Signal: "layers"},
	{Name: "lrscheduler (coarse)", Delivery: OCICoarse, Signal: "layers"},
	{Name: "vanilla (fine)", Delivery: OCIFine, Signal: "image"},
	{Name: "vanilla (coarse)", Delivery: OCICoarse, Signal: "image"},
}

// Result is the whole comparison.
type Result struct {
	ImageLocalitySizeSource string      `json:"imageLocalitySizeSource"`
	ImageLocalityNodes      int         `json:"imageLocalityNodes"`
	Nodes                   []Node      `json:"nodes"`
	Deliveries              []string    `json:"deliveries"`
	Matrix                  []Cell      `json:"matrix"`
	Ladder                  []Step      `json:"ladder"`
	Arms                    []Arm       `json:"arms"`
	Placement               []Placement `json:"placement"`
	// Overhead is the path-less layer every image carries, left out of
	// the OCI units above. A node that holds any image already has it.
	Overhead *Overhead `json:"overhead,omitempty"`
}

// Overhead describes the shared path-less layer.
type Overhead struct {
	DiffID   string `json:"diffID"`
	Size     uint64 `json:"size"`
	GzipSize uint64 `json:"gzipSize"`
}

// Run computes every table.
func (in Inputs) Run() (Result, error) {
	if err := in.Check(); err != nil {
		return Result{}, err
	}
	r := Result{Nodes: Nodes, Deliveries: Deliveries}
	for _, a := range Arms {
		if a.Signal != "image" || in.RuntimeImageSizes != nil {
			r.Arms = append(r.Arms, a)
		}
	}
	r.ImageLocalityNodes = in.imageLocalityNodes()
	r.ImageLocalitySizeSource = "unavailable: runtime image sizes required; vanilla arms omitted"
	if in.RuntimeImageSizes != nil {
		r.ImageLocalitySizeSource = "runtime-reported image sizes"
	}
	if l, ok := in.Overhead(); ok {
		r.Overhead = &Overhead{DiffID: l.DiffID, Size: l.Size, GzipSize: l.GzipSize}
	}
	for _, n := range Nodes {
		for _, t := range in.Labels() {
			for _, d := range Deliveries {
				raw, err := in.Missing(d, n.Holds, t, false)
				if err != nil {
					return Result{}, err
				}
				gz, err := in.Missing(d, n.Holds, t, true)
				if err != nil {
					return Result{}, err
				}
				r.Matrix = append(r.Matrix, Cell{
					Node: n.Name, Target: t, Delivery: d,
					Units: Bytes{uint64(len(raw.MissingPaths)), uint64(raw.TotalPaths)},
					Raw:   Bytes{raw.MissingBytes, raw.TotalBytes},
					Gzip:  Bytes{gz.MissingBytes, gz.TotalBytes},
				})
			}
		}
	}
	for _, s := range Steps {
		for _, d := range Deliveries {
			raw, err := in.Missing(d, []string{s[0]}, s[1], false)
			if err != nil {
				return Result{}, err
			}
			gz, err := in.Missing(d, []string{s[0]}, s[1], true)
			if err != nil {
				return Result{}, err
			}
			r.Ladder = append(r.Ladder, Step{
				From: s[0], To: s[1], Delivery: d,
				Raw:  Bytes{raw.MissingBytes, raw.TotalBytes},
				Gzip: Bytes{gz.MissingBytes, gz.TotalBytes},
			})
		}
	}
	for _, t := range in.Labels() {
		for _, a := range r.Arms {
			p, err := in.place(a, t)
			if err != nil {
				return Result{}, err
			}
			r.Placement = append(r.Placement, p)
		}
	}
	return r, nil
}

// place scores every node for target the way arm a's scheduler would, keeps
// the best-scoring nodes, and reports what they transfer under a's
// delivery. Schedulers and runtimes see whole images, so OCI arms score and
// transfer every layer, the path-less one included. Resource fit is equal
// on the simulated nodes, so it never breaks a tie.
func (in Inputs) place(a Arm, target string) (Placement, error) {
	if a.Signal == "image" && in.RuntimeImageSizes == nil {
		return Placement{}, fmt.Errorf("ImageLocality requires runtime image sizes")
	}
	p := Placement{Target: target, Arm: a.Name, Delivery: a.Delivery, Scores: map[string]float64{}, TransferByNode: map[string]uint64{}}
	type scored struct {
		node     string
		score    float64
		transfer uint64
	}
	var all []scored
	best := -1.0
	for _, n := range Nodes {
		m, err := in.MissingWhole(a.Delivery, n.Holds, target, false)
		if err != nil {
			return Placement{}, err
		}
		var s float64
		switch a.Signal {
		case "paths", "layers":
			// Fraction of the target's bytes already on the node: the
			// k3snix score and LRScheduler's layer term rank the same way.
			s = float64(m.HaveBytes) / float64(m.TotalBytes)
		case "image":
			if holds(n, target) {
				size, err := in.imageLocalitySize(a.Delivery, target)
				if err != nil {
					return Placement{}, err
				}
				if p.ImageSizes == nil {
					p.ImageSizes = map[string]uint64{}
				}
				p.ImageSizes[n.Name] = size
				s = float64(ImageLocalityScore(size, nodesHolding(target), in.imageLocalityNodes()))
			}
		case "none":
		default:
			return Placement{}, fmt.Errorf("arm %s: unknown signal %q", a.Name, a.Signal)
		}
		p.Scores[n.Name] = s
		p.TransferByNode[n.Name] = m.MissingBytes
		all = append(all, scored{n.Name, s, m.MissingBytes})
		if s > best {
			best = s
		}
	}
	first := true
	for _, s := range all {
		if s.score != best {
			continue
		}
		p.Picked = append(p.Picked, s.node)
		if first || s.transfer < p.TransferMin {
			p.TransferMin = s.transfer
		}
		if first || s.transfer > p.TransferMax {
			p.TransferMax = s.transfer
		}
		first = false
	}
	return p, nil
}

func (in Inputs) imageLocalityNodes() int {
	if in.ImageLocalityNodes != 0 {
		return in.ImageLocalityNodes
	}
	return len(Nodes)
}

func (in Inputs) imageLocalitySize(delivery, target string) (uint64, error) {
	size := in.RuntimeImageSizes[granularity[delivery]][target]
	if size == 0 {
		return 0, fmt.Errorf("no runtime image size for %s/%s", delivery, target)
	}
	return size, nil
}

func holds(n Node, label string) bool {
	for _, h := range n.Holds {
		if h == label {
			return true
		}
	}
	return false
}

func nodesHolding(label string) int {
	c := 0
	for _, n := range Nodes {
		if holds(n, label) {
			c++
		}
	}
	return c
}

// ImageLocality's constants and score, as in kube-scheduler's imagelocality
// plugin (pkg/scheduler/framework/plugins/imagelocality, unchanged from
// v1.32 through v1.36 (the K3s in the dev shell)): the image size is scaled
// by the fraction of nodes that hold the image, clamped to [23 MiB,
// 1000 MiB per container], and mapped linearly onto 0..100. A node without
// the image scores 0. size is what the runtime reports for the image;
// use RuntimeImageSizes for a particular runtime rather than assuming its
// accounting equals the compressed layer sum used by the offline model.
const (
	imageLocalityMin = 23 << 20
	imageLocalityMax = 1000 << 20 // one container
	maxNodeScore     = 100
)

// ImageLocalityScore is the score of a node that holds a single-container
// pod's image of size bytes, held by nodesWith of totalNodes nodes.
func ImageLocalityScore(size uint64, nodesWith, totalNodes int) int64 {
	sum := int64(float64(size) * (float64(nodesWith) / float64(totalNodes)))
	if sum < imageLocalityMin {
		sum = imageLocalityMin
	} else if sum > imageLocalityMax {
		sum = imageLocalityMax
	}
	return maxNodeScore * (sum - imageLocalityMin) / (imageLocalityMax - imageLocalityMin)
}

// Markdown writes the tables of r.
func (r Result) Markdown(w io.Writer) {
	fmt.Fprintf(w, "ImageLocality: %s; %d total nodes including filtered nodes.\n\n", r.ImageLocalitySizeSource, r.ImageLocalityNodes)
	if r.Overhead != nil {
		fmt.Fprintf(w, "Every image also carries one path-less layer, %s (%s raw, %s gzip), identical in all images and left out of the OCI rows below.\n\n",
			r.Overhead.DiffID, human(r.Overhead.Size), human(r.Overhead.GzipSize))
	}
	fmt.Fprintln(w, "## Missing bytes per node (raw: NAR bytes / layer tar bytes)")
	fmt.Fprintln(w)
	r.matrixTable(w, func(c Cell) Bytes { return c.Raw })
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Missing bytes per node (gzip, default level, same compressor for NARs and layers)")
	fmt.Fprintln(w)
	r.matrixTable(w, func(c Cell) Bytes { return c.Gzip })
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Missing units per node (store paths / layers)")
	fmt.Fprintln(w)
	r.unitTable(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Churn ladder: bytes a node on the previous version transfers")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "| step | %s |\n", strings.Join(r.Deliveries, " | "))
	fmt.Fprintf(w, "|---|%s\n", strings.Repeat("---|", len(r.Deliveries)))
	for i := 0; i < len(r.Ladder); i += len(r.Deliveries) {
		s := r.Ladder[i]
		fmt.Fprintf(w, "| %s → %s |", s.From, s.To)
		for _, c := range r.Ladder[i : i+len(r.Deliveries)] {
			fmt.Fprintf(w, " %s (gzip %s) |", cellText(c.Raw), human(c.Gzip.Missing))
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## Placement: preferred nodes and raw bytes then transferred (OCI: whole images)")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| target | arm | delivery | preferred node(s) | transfer |")
	fmt.Fprintln(w, "|---|---|---|---|---|")
	for _, p := range r.Placement {
		picked := strings.Join(p.Picked, ", ")
		if len(p.Picked) == len(r.Nodes) {
			picked = "tie (all)"
		}
		tr := human(p.TransferMin)
		if p.TransferMax != p.TransferMin {
			tr = human(p.TransferMin) + " to " + human(p.TransferMax)
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n", p.Target, p.Arm, p.Delivery, picked, tr)
	}
}

func (r Result) matrixTable(w io.Writer, pick func(Cell) Bytes) {
	fmt.Fprint(w, "| target | delivery |")
	for _, n := range r.Nodes {
		fmt.Fprintf(w, " %s |", n.Name)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "|---|---|%s\n", strings.Repeat("---|", len(r.Nodes)))
	for _, target := range r.targets() {
		for _, d := range r.Deliveries {
			fmt.Fprintf(w, "| %s | %s |", target, d)
			for _, n := range r.Nodes {
				fmt.Fprintf(w, " %s |", cellText(pick(r.cell(n.Name, target, d))))
			}
			fmt.Fprintln(w)
		}
	}
}

func (r Result) unitTable(w io.Writer) {
	fmt.Fprint(w, "| target | delivery |")
	for _, n := range r.Nodes {
		fmt.Fprintf(w, " %s |", n.Name)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "|---|---|%s\n", strings.Repeat("---|", len(r.Nodes)))
	for _, target := range r.targets() {
		for _, d := range r.Deliveries {
			fmt.Fprintf(w, "| %s | %s |", target, d)
			for _, n := range r.Nodes {
				u := r.cell(n.Name, target, d).Units
				fmt.Fprintf(w, " %d of %d |", u.Missing, u.Total)
			}
			fmt.Fprintln(w)
		}
	}
}

func (r Result) targets() []string {
	var ts []string
	seen := map[string]bool{}
	for _, c := range r.Matrix {
		if !seen[c.Target] {
			seen[c.Target] = true
			ts = append(ts, c.Target)
		}
	}
	return ts
}

func (r Result) cell(node, target, d string) Cell {
	for _, c := range r.Matrix {
		if c.Node == node && c.Target == target && c.Delivery == d {
			return c
		}
	}
	return Cell{}
}

func cellText(b Bytes) string {
	if b.Total == 0 {
		return "-"
	}
	return fmt.Sprintf("%s of %s (%.1f%%)", human(b.Missing), human(b.Total), float64(b.Missing)*100/float64(b.Total))
}

func human(n uint64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
