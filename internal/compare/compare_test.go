package compare

import (
	"bytes"
	"strings"
	"testing"

	"k3snix/internal/closure"
	"k3snix/internal/oci"
)

const (
	glibc = "/nix/store/aaaa-glibc"
	hello = "/nix/store/bbbb-hello"
	core  = "/nix/store/cccc-coreutils"
	other = "/nix/store/dddd-other"
)

// fixture: share-a = {glibc, hello}, share-b = {glibc, coreutils}, and
// churn-v1 = {other}. Fine images are one layer per path; coarse images
// one layer for all; both end with the same path-less layer.
func fixture() Inputs {
	size := map[string]uint64{glibc: 1000, hello: 100, core: 400, other: 50}
	man := map[string][]string{
		"churn-v1": {other},
		"share-a":  {glibc, hello},
		"share-b":  {core, glibc},
	}
	extra := oci.Layer{DiffID: "sha256:extra", Size: 10, GzipSize: 1}
	in := Inputs{
		Manifests: map[string]closure.Closure{},
		NarGzip:   map[string]uint64{},
		Images:    map[string]map[string]oci.Image{"fine": {}, "coarse": {}},
	}
	for p, s := range size {
		in.NarGzip[p] = s / 10
	}
	for label, paths := range man {
		c := closure.Closure{Label: label}
		fine := oci.Image{Label: label}
		var total uint64
		for _, p := range paths {
			c.Paths = append(c.Paths, closure.Path{Path: p, NarSize: size[p]})
			// a layer tar is a little larger than the NAR
			fine.Layers = append(fine.Layers, oci.Layer{DiffID: "sha256:fine" + p, Size: size[p] + 1, GzipSize: size[p] / 10, Paths: []string{p}})
			total += size[p] + 1
		}
		fine.Layers = append(fine.Layers, extra)
		coarse := oci.Image{Label: label, Layers: []oci.Layer{
			{DiffID: "sha256:coarse-" + label, Size: total, GzipSize: total / 10, Paths: paths},
			extra,
		}}
		in.Manifests[label] = c
		in.Images["fine"][label] = fine
		in.Images["coarse"][label] = coarse
	}
	return in
}

func TestMissingSharesGlibcByDelivery(t *testing.T) {
	in := fixture()
	if err := in.Check(); err != nil {
		t.Fatal(err)
	}
	held := []string{"share-a"}
	for _, tc := range []struct {
		d            string
		gz           bool
		missing, tot uint64
	}{
		{Nix, false, 400, 1400},
		{Nix, true, 40, 140},
		{OCIFine, false, 401, 1402}, // the shared path-less layer is left out
		{OCICoarse, false, 1402, 1402},
	} {
		m, err := in.Missing(tc.d, held, "share-b", tc.gz)
		if err != nil {
			t.Fatal(err)
		}
		if m.MissingBytes != tc.missing || m.TotalBytes != tc.tot {
			t.Errorf("%s gz=%v: missing %d of %d, want %d of %d", tc.d, tc.gz, m.MissingBytes, m.TotalBytes, tc.missing, tc.tot)
		}
	}
}

func TestCheckRejectsImageThatDiffersFromManifest(t *testing.T) {
	in := fixture()
	img := in.Images["fine"]["share-a"]
	img.Layers = append([]oci.Layer{{DiffID: "sha256:x", Size: 1, Paths: []string{core}}}, img.Layers...)
	in.Images["fine"]["share-a"] = img
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "image adds "+core) {
		t.Fatalf("want 'image adds', got %v", err)
	}

	in = fixture()
	img = in.Images["coarse"]["share-b"]
	img.Layers[0].Paths = []string{glibc}
	in.Images["coarse"]["share-b"] = img
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "image lacks "+core) {
		t.Fatalf("want 'image lacks', got %v", err)
	}
}

func TestCheckRejectsLayering(t *testing.T) {
	in := fixture()
	img := in.Images["fine"]["share-a"]
	img.Layers = []oci.Layer{{DiffID: "sha256:both", Size: 2, Paths: []string{glibc, hello}}}
	in.Images["fine"]["share-a"] = img
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "holds 2 store paths") {
		t.Fatalf("want fine layering error, got %v", err)
	}

	in = fixture()
	img = in.Images["coarse"]["share-a"]
	img.Layers = []oci.Layer{
		{DiffID: "sha256:a", Paths: []string{glibc}},
		{DiffID: "sha256:b", Paths: []string{hello}},
	}
	in.Images["coarse"]["share-a"] = img
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "want 1") {
		t.Fatalf("want coarse layering error, got %v", err)
	}
}

func TestCheckRejectsDifferentPathlessLayers(t *testing.T) {
	in := fixture()
	img := in.Images["fine"]["share-b"]
	img.Layers[len(img.Layers)-1] = oci.Layer{DiffID: "sha256:other-extra"}
	in.Images["fine"]["share-b"] = img
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "second path-less layer") {
		t.Fatalf("want path-less layer error, got %v", err)
	}
}

func TestCheckRequiresNarGzip(t *testing.T) {
	in := fixture()
	delete(in.NarGzip, hello)
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "gzipped NAR") {
		t.Fatalf("want gzip error, got %v", err)
	}
}

func TestCheckRejectsUnstableSharedFineLayer(t *testing.T) {
	in := fixture()
	img := in.Images["fine"]["share-b"]
	for i := range img.Layers {
		if len(img.Layers[i].Paths) == 1 && img.Layers[i].Paths[0] == glibc {
			img.Layers[i].DiffID = "sha256:glibc-with-different-metadata"
		}
	}
	in.Images["fine"]["share-b"] = img
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "inconsistent diffIDs") {
		t.Fatalf("accepted different fine layers for the same shared store path: %v", err)
	}
}

func TestPlace(t *testing.T) {
	in := fixture()
	in.RuntimeImageSizes = map[string]map[string]uint64{"fine": {}, "coarse": {}}
	for _, g := range []string{"fine", "coarse"} {
		for _, label := range in.Labels() {
			in.RuntimeImageSizes[g][label] = 1000
		}
	}
	byArm := func(name string) Arm {
		for _, a := range Arms {
			if a.Name == name {
				return a
			}
		}
		t.Fatalf("no arm %s", name)
		return Arm{}
	}
	for _, tc := range []struct {
		arm, target string
		picked      string
		min, max    uint64
	}{
		// k3snix and the fine layer score both find the glibc node.
		{"k3snix", "share-b", "node-hello", 400, 400},
		{"lrscheduler (fine)", "share-b", "node-hello", 401, 401},
		// One coarse layer shares no workload content, but the seeded
		// nodes hold the shared path-less layer, as a real layer scorer
		// sees; the empty node also has to fetch it.
		{"lrscheduler (coarse)", "share-b", "node-churn-v1,node-hello", 1402, 1402},
		// ImageLocality floors image sizes at 23 MiB: these images are
		// far smaller, so even the node holding the exact image scores 0.
		{"vanilla (fine)", "share-b", "node-churn-v1,node-hello,node-empty", 401, 1412},
		{"vanilla (fine)", "share-a", "node-churn-v1,node-hello,node-empty", 0, 1112},
		{"default-hoststore", "share-b", "node-churn-v1,node-hello,node-empty", 400, 1400},
	} {
		p, err := in.place(byArm(tc.arm), tc.target)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(p.Picked, ","); got != tc.picked || p.TransferMin != tc.min || p.TransferMax != tc.max {
			t.Errorf("%s/%s: picked %s, transfer %d..%d; want %s, %d..%d",
				tc.arm, tc.target, got, p.TransferMin, p.TransferMax, tc.picked, tc.min, tc.max)
		}
	}
}

func TestImageLocalityScore(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		size        uint64
		with, total int
		want        int64
	}{
		{13 * mib, 1, 3, 0},     // hello-sized: under the 23 MiB floor
		{60 * mib, 1, 3, 0},     // scaled to 20 MiB: still under it
		{60 * mib, 3, 3, 3},     // 100*(60-23)/(1000-23) = 3.8, truncated
		{300 * mib, 1, 3, 7},    // scaled to 100 MiB: 100*77/977 = 7.9
		{3000 * mib, 3, 3, 100}, // clamped at 1000 MiB
	} {
		if got := ImageLocalityScore(tc.size, tc.with, tc.total); got != tc.want {
			t.Errorf("ImageLocalityScore(%d MiB, %d/%d) = %d, want %d", tc.size/mib, tc.with, tc.total, got, tc.want)
		}
	}
}

func TestImageLocalitySizeDefinitions(t *testing.T) {
	in := fixture()
	arm := Arm{Name: "vanilla", Delivery: OCIFine, Signal: "image"}
	for _, target := range []string{"share-a", "share-b"} {
		if _, err := in.place(arm, target); err == nil || !strings.Contains(err.Error(), "requires runtime image sizes") {
			t.Fatalf("guessed image score for %s without runtime sizes: %v", target, err)
		}
	}
	in.RuntimeImageSizes = map[string]map[string]uint64{"fine": {}, "coarse": {}}
	for _, g := range []string{"fine", "coarse"} {
		for _, label := range in.Labels() {
			in.RuntimeImageSizes[g][label] = 300 << 20
		}
	}
	in.ImageLocalityNodes = 4 // tainted server still counts in the snapshot
	if err := in.Check(); err != nil {
		t.Fatal(err)
	}
	p, err := in.place(arm, "share-a")
	if err != nil {
		t.Fatal(err)
	}
	if p.Scores["node-hello"] != 5 || p.ImageSizes["node-hello"] != 300<<20 || strings.Join(p.Picked, ",") != "node-hello" {
		t.Fatalf("runtime size/count not used: %+v", p)
	}
	delete(in.RuntimeImageSizes["coarse"], "share-a")
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "no runtime image size") {
		t.Fatalf("incomplete sizes accepted: %v", err)
	}
	in.ImageLocalityNodes = 2
	if err := in.Check(); err == nil || !strings.Contains(err.Error(), "node count") {
		t.Fatalf("incomplete node count accepted: %v", err)
	}
}

func TestRunAndMarkdown(t *testing.T) {
	in := fixture()
	// Run walks Steps, which name the real churn labels.
	for _, l := range []string{"churn-v2", "churn-v3", "churn-v4"} {
		in.Manifests[l] = in.Manifests["churn-v1"]
		in.Images["fine"][l] = in.Images["fine"]["churn-v1"]
		in.Images["coarse"][l] = in.Images["coarse"]["churn-v1"]
	}
	r, err := in.Run()
	if err != nil {
		t.Fatal(err)
	}
	if r.Overhead == nil || r.Overhead.DiffID != "sha256:extra" {
		t.Errorf("overhead: %+v", r.Overhead)
	}
	for _, a := range r.Arms {
		if a.Signal == "image" {
			t.Fatal("offline result includes ImageLocality without runtime sizes")
		}
	}
	for _, p := range r.Placement {
		if strings.HasPrefix(p.Arm, "vanilla") {
			t.Fatal("offline result includes guessed vanilla placement")
		}
	}
	if want := len(Nodes) * len(in.Labels()) * len(Deliveries); len(r.Matrix) != want {
		t.Errorf("matrix has %d cells, want %d", len(r.Matrix), want)
	}
	var buf bytes.Buffer
	r.Markdown(&buf)
	for _, s := range []string{"## Churn ladder", "| share-b | nix |", "tie (all)", "sha256:extra"} {
		if !strings.Contains(buf.String(), s) {
			t.Errorf("markdown lacks %q", s)
		}
	}
}

// Whole-image transfers include the path-less layer only on an empty node.
func TestPlacementTransferByNode(t *testing.T) {
	in := fixture()
	for _, d := range Deliveries {
		p, err := in.place(Arm{Name: "test", Delivery: d, Signal: "none"}, "share-b")
		if err != nil {
			t.Fatal(err)
		}
		if len(p.TransferByNode) != len(Nodes) {
			t.Fatalf("incomplete transfers: %+v", p)
		}
		for _, n := range Nodes {
			want, err := in.MissingWhole(d, n.Holds, "share-b", false)
			if err != nil {
				t.Fatal(err)
			}
			if p.TransferByNode[n.Name] != want.MissingBytes {
				t.Errorf("%s/%s: got %d want %d", d, n.Name, p.TransferByNode[n.Name], want.MissingBytes)
			}
		}
		if d == OCIFine && (p.TransferByNode["node-hello"] != 401 || p.TransferByNode["node-empty"] != 1412) {
			t.Fatalf("whole image overhead: %+v", p)
		}
	}
}
