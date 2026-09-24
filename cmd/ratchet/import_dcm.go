package main

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/sherzing/assay/internal/dcm"
	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

// pickFormat honours an explicit --format and otherwise sniffs the document.
func pickFormat(flagVal string, data []byte) string {
	if flagVal != "auto" {
		return flagVal
	}
	if dcm.Sniff(data) {
		return "dcm"
	}
	return "sarif"
}

// source is one input argument: a file, optionally with the directory DCM ran in as file=prefix,
// so the reports of several packages import in one invocation and roll up once.
type source struct {
	file, prefix string
	explicit     bool
}

// parseSource splits file=prefix; --prefix is the default for a report without one.
func parseSource(arg, def string) source {
	if i := strings.LastIndex(arg, "="); i > 0 {
		return source{file: arg[:i], prefix: arg[i+1:], explicit: true}
	}
	return source{file: arg, prefix: def}
}

// decodeDCM reads one DCM document; the merge and the notes happen once every document is in.
func (in *imported) decodeDCM(data []byte, o importOpts, prefix string) error {
	res, err := dcm.Import(bytes.NewReader(data), dcm.Options{Root: o.root, Prefix: prefix})
	if err != nil {
		return err
	}
	in.dcms = append(in.dcms, res)
	return nil
}

// addDCM merges the DCM documents into the report and says what the import could not use.
func (in *imported) addDCM() error {
	if len(in.dcms) == 0 {
		return nil
	}
	res, err := dcm.Merge(in.dcms...)
	if err != nil {
		return err
	}
	in.dcm = res
	in.rep.Funcs = res.Functions
	if res.FormatVersion != dcm.Version {
		fmt.Fprintf(os.Stderr, "note: DCM report is formatVersion %d; this importer was verified against %d\n",
			res.FormatVersion, dcm.Version)
	}
	if res.Issues > 0 {
		fmt.Fprintf(os.Stderr, "not imported: %d DCM lint issues (findings are not supported yet, only metrics)\n", res.Issues)
	}
	if res.Generated > 0 {
		fmt.Fprintf(os.Stderr, "skipped %d generated files\n", res.Generated)
	}
	if res.Located == 0 {
		fmt.Fprintf(os.Stderr, "none of the report's %d files is under --root; generated-file headers were not checked "+
			"(did DCM run in a package? see --prefix)\n", res.Files)
	}
	return nil
}

// reportDCM says what was measured.
func (in *imported) reportDCM() {
	if in.dcm == nil {
		return
	}
	fmt.Printf("measured %d declarations in %d files, %d measures\n", in.dcm.Funcs, in.dcm.Files, len(in.dcm.Measures))
}

// gateable refuses a check that could not fail: DCM input has no findings, so only the caps can hold.
func (in *imported) gateable(o importOpts) error {
	if in.dcm != nil && in.sarifs == 0 && !o.strictCaps {
		return fmt.Errorf("nothing to gate: DCM findings are not imported yet; add --strict-caps to hold the complexity caps")
	}
	return nil
}

// dcmMeasures is what --emit measures writes for DCM input: the project series derived here, the
// way the scan derives its own from its records, and the declarations unless --detail=false.
func (in *imported) dcmMeasures(detail bool) []schema.Measure {
	if in.dcm == nil {
		return nil
	}
	out := dcmSeries(in.dcm)
	if detail {
		out = append(out, in.dcm.Measures...)
	}
	return out
}

// dcmSeries rolls the function-level values up to project scope under the names the scan emits.
func dcmSeries(res *dcm.Result) []schema.Measure {
	byName := map[string][]float64{}
	for _, m := range res.Measures {
		if m.Scope == schema.ScopeFunction {
			byName[m.Metric] = append(byName[m.Metric], m.Value)
		}
	}
	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []schema.Measure
	put := func(name string, v float64) {
		out = append(out, schema.Measure{Scope: schema.ScopeProject, Metric: name, Value: v})
	}
	for _, n := range names {
		d := model.NewFloatDist(byName[n])
		put(n+".max", d.Max)
		put(n+".mean", d.Mean)
		put(n+".p50", d.P50)
		put(n+".p90", d.P90)
	}
	put("files", float64(res.Files))
	put("funcs", float64(res.Funcs))
	return out
}
