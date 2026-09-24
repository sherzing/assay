package dcm

import (
	"strings"
	"testing"

	"github.com/sherzing/assay/pkg/schema"
)

func TestIdentitiesDcmRepeats(t *testing.T) {
	res := imp(t, report, Options{})
	want := map[string]float64{
		"lib/a.dart:Box.value":        4, // getter
		"lib/a.dart:Box.value=":       1, // setter, as Dart names it
		"lib/a.dart:Box.work.inner":   2, // local function, qualified by what encloses it
		"lib/a.dart:Unnamed.triple":   1, // method of an unnamed extension
		"lib/a.dart:Unnamed#2.triple": 1,
		"lib/a.dart:topLevel":         2,
	}
	for path, v := range want {
		if got := measure(t, res, schema.ScopeFunction, path, "cyclomatic"); got != v {
			t.Errorf("%s cyclomatic = %v, want %v", path, got, v)
		}
	}
	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Box.value=", "nesting"); v != 2 {
		t.Errorf("setter nesting = %v, want 2", v)
	}
	if v := measure(t, res, schema.ScopeClass, "lib/a.dart:Unnamed#2", "methods"); v != 1 {
		t.Errorf("second unnamed extension methods = %v, want 1", v)
	}
	names := map[string]bool{}
	for _, f := range res.Functions {
		names[f.Name] = true
	}
	for _, n := range []string{"Box.value", "Box.value=", "Box.work.inner", "Unnamed#2.triple"} {
		if !names[n] {
			t.Errorf("no per-function record named %q; the records must use the same identities as the measures", n)
		}
	}
}

func TestRecordsOnlyForDeclarationsWithCyclomatic(t *testing.T) {
	res := imp(t, report, Options{})
	if len(res.Functions) != 8 || res.Funcs != 8 {
		t.Fatalf("functions = %d / %d, want the 8 declarations DCM computed cyclomatic for", len(res.Functions), res.Funcs)
	}
	for _, f := range res.Functions {
		if f.Name == "Box.Box" || f.Name == "Box.clear" {
			t.Errorf("%s has no body, yet got a record with cyclomatic %d", f.Name, f.Cyclomatic)
		}
	}
	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Box.clear", "params"); v != 0 {
		t.Errorf("Box.clear params = %v; a bodiless declaration still has its own measures", v)
	}
	get := res.Functions[0]
	if get.Name != "Box.value" || get.File != "lib/a.dart" || get.Line != 7 || get.Cyclomatic != 4 || !get.Exported {
		t.Errorf("getter record = %+v", get)
	}
	if set := res.Functions[1]; set.Name != "Box.value=" || set.Cyclomatic != 1 || set.MaxNesting != 2 || set.Cognitive != 0 {
		t.Errorf("setter record = %+v; cognitive must stay 0, DCM has no such metric", set)
	}
	for _, f := range res.Functions {
		if f.Name == "Box.work" && f.Statements != 0 {
			t.Errorf("Box.work statements = %d; sloc is not a statement count", f.Statements)
		}
	}
}

func TestMeasuresScopesAndNames(t *testing.T) {
	res := imp(t, report, Options{})
	if res.FormatVersion != Version {
		t.Errorf("formatVersion = %d, want %d", res.FormatVersion, Version)
	}
	if v := measure(t, res, schema.ScopeClass, "lib/a.dart:Box", "weight"); v != 0.5 {
		t.Errorf("Box weight = %v, want 0.5", v)
	}
	if v := measure(t, res, schema.ScopeFile, "lib/a.dart", "imports"); v != 1 {
		t.Errorf("imports = %v, want 1", v)
	}
	// Unknown metrics import too: dashes become dots, quoted values still parse.
	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Box.work", "some.new.metric"); v != 2.5 {
		t.Errorf("unknown metric = %v, want 2.5", v)
	}
	for _, m := range res.Measures {
		if m.Scope == schema.ScopeProject {
			t.Errorf("project-scope %s in the result; roll-ups are the caller's", m.Metric)
		}
	}
	if res.Files != 1 || res.Issues != 5 {
		t.Errorf("files = %d issues = %d, want 1 and 5 (2 lint, 1 unused file, 1 unused code, 1 duplication)", res.Files, res.Issues)
	}
}

func TestPrefixRebasesPaths(t *testing.T) {
	res := imp(t, report, Options{Prefix: "packages/edge/"})
	if v := measure(t, res, schema.ScopeFunction, "packages/edge/lib/a.dart:Box.work", "cyclomatic"); v != 3 {
		t.Errorf("prefixed Box.work cyclomatic = %v, want 3", v)
	}
	if f := res.Functions[0].File; f != "packages/edge/lib/a.dart" {
		t.Errorf("record file = %q, want the prefixed path", f)
	}
}

func TestGeneratedFilesAreSkipped(t *testing.T) {
	root := t.TempDir()
	write(t, root, "lib/headed.dart", "// GENERATED CODE - DO NOT MODIFY BY HAND\nint f() => 1;\n")
	one := func(file string) string {
		return `{"path":"` + file + `","issues":[{"id":"cyclomatic-complexity","location":{"startLine":2},"value":1,"declarationName":"f","declarationType":"function"}]}`
	}
	doc := `{"formatVersion":13,"metricResults":[` + one("lib/model.pb.dart") + "," + one("lib/api/service.pbgrpc.dart") + "," +
		one("lib/generated/x.dart") + "," + one("lib/headed.dart") + "," + one("lib/a.dart") + `]}`
	res := imp(t, doc, Options{Root: root})
	if res.Generated != 4 || res.Files != 1 || len(res.Measures) != 1 || res.Measures[0].Path != "lib/a.dart:f" {
		t.Errorf("generated = %d files = %d measures = %+v", res.Generated, res.Files, res.Measures)
	}
	if res.Located != 1 {
		t.Errorf("located = %d, want the one file that is on disk", res.Located)
	}
	if res := imp(t, doc, Options{}); res.Generated != 3 || res.Located != 0 {
		t.Errorf("without Root the header cannot be read: generated = %d located = %d, want 3 and 0", res.Generated, res.Located)
	}
	only := `{"formatVersion":13,"metricResults":[` + one("lib/model.pb.dart") + `]}`
	if _, err := Import(strings.NewReader(only), Options{}); err == nil || !strings.Contains(err.Error(), "generated") {
		t.Errorf("a report of only generated code: err = %v", err)
	}
}

func TestMergeUnionsAndRefusesDisagreement(t *testing.T) {
	a := imp(t, report, Options{})
	same, err := Merge(a, imp(t, report, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if same.Files != 1 || same.Funcs != a.Funcs || len(same.Measures) != len(a.Measures) {
		t.Errorf("the same report twice = %d files, %d funcs, %d measures; want a's %d, %d, %d",
			same.Files, same.Funcs, len(same.Measures), a.Files, a.Funcs, len(a.Measures))
	}
	other := imp(t, strings.ReplaceAll(report, "lib/a.dart", "lib/b.dart"), Options{})
	union, err := Merge(a, other)
	if err != nil {
		t.Fatal(err)
	}
	if union.Files != 2 || union.Funcs != 16 || len(union.Measures) != 2*len(a.Measures) {
		t.Errorf("union = %d files, %d funcs, %d measures", union.Files, union.Funcs, len(union.Measures))
	}
	conflict := imp(t, strings.ReplaceAll(report, `"value":25`, `"value":9`), Options{})
	if _, err := Merge(a, conflict); err == nil || !strings.Contains(err.Error(), "--prefix") {
		t.Errorf("two values for one identity: err = %v, want a refusal pointing at --prefix", err)
	}
	if one, err := Merge(a); err != nil || one != a {
		t.Errorf("a single report merges to itself")
	}
}

// DCM reports no nesting for a constructor whose only logic is an initialiser list, yet gives it a
// cyclomatic value. The Go scan says 0 for a flat function, so the measure says 0 too, but only when
// the report measures nesting at all: an unconfigured metric is not a zero.
func TestNestingIsZeroForRecordsTheReportLeftOut(t *testing.T) {
	doc := `{"formatVersion":13,"metricResults":[{"path":"lib/a.dart","issues":[
	 {"id":"cyclomatic-complexity","location":{"startLine":2,"endLine":4},"value":3,"declarationName":"a","declarationType":"function"},
	 {"id":"maximum-nesting-level","location":{"startLine":2,"endLine":4},"value":1,"declarationName":"a","declarationType":"function"},
	 {"id":"cyclomatic-complexity","location":{"startLine":6,"endLine":6},"value":2,"declarationName":"Box.Box","declarationType":"constructor"}]}]}`
	res := imp(t, doc, Options{})
	if v := measure(t, res, schema.ScopeFunction, "lib/a.dart:Box.Box", "nesting"); v != 0 {
		t.Errorf("filled nesting = %v, want 0", v)
	}
	if len(res.Functions) != 2 || res.Functions[1].Name != "Box.Box" || res.Functions[1].MaxNesting != 0 {
		t.Errorf("records = %+v", res.Functions)
	}
	none := strings.Replace(doc, `"id":"maximum-nesting-level"`, `"id":"number-of-parameters"`, 1)
	if res := imp(t, none, Options{}); has(res, schema.ScopeFunction, "lib/a.dart:Box.Box", "nesting") {
		t.Error("a report that measures no nesting had a nesting value invented")
	}
}
