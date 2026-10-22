// Command k3snix-bench holds the offline measurements. Today it compares Nix
// store-path delivery with OCI layer delivery on the pinned workloads:
//
//	k3snix-bench oci-layers -label L -granularity G image.tar.gz > L.json
//	k3snix-bench nar-gzip -manifests DIR > nar-gzip.json
//	k3snix-bench compare -manifests DIR -images DIR -nar-gzip FILE \
//	    [-summary summary.json] [-json result.json] > tables.md
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"k3snix/internal/closure"
	"k3snix/internal/compare"
	"k3snix/internal/nar"
	"k3snix/internal/oci"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "oci-layers":
		err = ociLayers(os.Args[2:])
	case "nar-gzip":
		err = narGzip(os.Args[2:])
	case "compare":
		err = runCompare(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "k3snix-bench:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: k3snix-bench oci-layers|nar-gzip|compare [flags]")
	os.Exit(2)
}

func ociLayers(args []string) error {
	fs := flag.NewFlagSet("oci-layers", flag.ExitOnError)
	label := fs.String("label", "", "workload label")
	gran := fs.String("granularity", "", "fine or coarse")
	_ = fs.Parse(args)
	if fs.NArg() != 1 || *label == "" || *gran == "" {
		return fmt.Errorf("oci-layers: need -label, -granularity and one image archive")
	}
	img, err := oci.ReadArchive(fs.Arg(0))
	if err != nil {
		return err
	}
	img.Label, img.Granularity = *label, *gran
	return writeJSON(os.Stdout, img)
}

// narGzip dumps every path of every manifest with the pure-Go NAR writer
// and records the NAR's size through gzip at the default level, the same
// compressor oci-layers applies to layer tars. The raw NAR size is checked
// against the manifest's narSize on the way.
func narGzip(args []string) error {
	fs := flag.NewFlagSet("nar-gzip", flag.ExitOnError)
	dir := fs.String("manifests", "", "closure manifest directory")
	_ = fs.Parse(args)
	cat, err := loadManifests(*dir)
	if err != nil {
		return err
	}
	sizes := map[string]uint64{}
	for _, c := range cat {
		for _, p := range c.Paths {
			if _, ok := sizes[p.Path]; ok {
				continue
			}
			raw, gz := &counter{}, &counter{}
			zw := gzip.NewWriter(gz)
			if err := nar.Write(io.MultiWriter(raw, zw), p.Path); err != nil {
				return fmt.Errorf("nar %s: %w", p.Path, err)
			}
			if err := zw.Close(); err != nil {
				return err
			}
			if raw.n != p.NarSize {
				return fmt.Errorf("nar %s: %d bytes, manifest narSize %d", p.Path, raw.n, p.NarSize)
			}
			sizes[p.Path] = gz.n
		}
	}
	return writeJSON(os.Stdout, sizes)
}

func runCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	manDir := fs.String("manifests", "", "closure manifest directory")
	imgDir := fs.String("images", "", "directory of <granularity>/<label>.json from oci-layers")
	gzFile := fs.String("nar-gzip", "", "nar-gzip output")
	runtimeSizes := fs.String("runtime-image-sizes", "", "optional JSON: granularity -> label -> CRI image sizeBytes")
	localityNodes := fs.Int("image-locality-nodes", 0, "total cluster nodes including filtered nodes (default: model workers)")
	summary := fs.String("summary", "", "summary.json of the closures build, to cross-check the nix ladder")
	jsonOut := fs.String("json", "", "write the full result as JSON here")
	_ = fs.Parse(args)

	in := compare.Inputs{Images: map[string]map[string]oci.Image{}}
	in.ImageLocalityNodes = *localityNodes
	var err error
	if *runtimeSizes != "" {
		if err := readJSON(*runtimeSizes, &in.RuntimeImageSizes); err != nil {
			return err
		}
		if in.RuntimeImageSizes == nil {
			return fmt.Errorf("runtime image sizes must be an object")
		}
	}
	if in.Manifests, err = loadManifests(*manDir); err != nil {
		return err
	}
	if err := readJSON(*gzFile, &in.NarGzip); err != nil {
		return err
	}
	for _, g := range []string{"fine", "coarse"} {
		in.Images[g] = map[string]oci.Image{}
		for label := range in.Manifests {
			var img oci.Image
			if err := readJSON(filepath.Join(*imgDir, g, label+".json"), &img); err != nil {
				return err
			}
			in.Images[g][label] = img
		}
	}
	res, err := in.Run()
	if err != nil {
		return err
	}
	if *summary != "" {
		if err := checkLadder(*summary, res); err != nil {
			return err
		}
	}
	if *jsonOut != "" {
		f, err := os.Create(*jsonOut)
		if err != nil {
			return err
		}
		if err := writeJSON(f, res); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	res.Markdown(os.Stdout)
	return nil
}

// checkLadder requires the nix rows of the ladder to equal the transfer
// bytes the closures build computed independently, in jq.
func checkLadder(file string, res compare.Result) error {
	var s struct {
		Churn struct {
			Steps []struct {
				From, To      string
				TransferBytes uint64
			} `json:"steps"`
			FromV1 []struct {
				From, To      string
				TransferBytes uint64
			} `json:"fromV1"`
		} `json:"churn"`
	}
	if err := readJSON(file, &s); err != nil {
		return err
	}
	want := map[[2]string]uint64{}
	for _, st := range append(s.Churn.Steps, s.Churn.FromV1...) {
		want[[2]string{st.From, st.To}] = st.TransferBytes
	}
	for _, st := range res.Ladder {
		if st.Delivery != compare.Nix {
			continue
		}
		w, ok := want[[2]string{st.From, st.To}]
		if !ok {
			return fmt.Errorf("summary has no %s -> %s", st.From, st.To)
		}
		if st.Raw.Missing != w {
			return fmt.Errorf("%s -> %s: compare says %d, summary.json says %d", st.From, st.To, st.Raw.Missing, w)
		}
	}
	return nil
}

func loadManifests(dir string) (map[string]closure.Closure, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no manifests in %q", dir)
	}
	out := map[string]closure.Closure{}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		c, err := closure.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		out[c.Label] = c
	}
	return out, nil
}

func readJSON(file string, v any) error {
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", file, err)
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type counter struct{ n uint64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += uint64(len(p))
	return len(p), nil
}
