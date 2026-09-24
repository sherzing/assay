package main

import (
	"strconv"
	"testing"

	"github.com/sherzing/assay/cmd/internal/cmdtest"
)

func dcmReport(file string, cyc int) string {
	return `{"formatVersion":13,"metricResults":[{"path":"` + file + `","issues":[
  {"id":"cyclomatic-complexity","location":{"startLine":3,"endLine":5},"level":"none","threshold":20,"value":` + strconv.Itoa(cyc) + `,"declarationName":"Foo.add","declarationType":"method"},
  {"id":"widgets-nesting-level","location":{"startLine":3,"endLine":5},"level":"none","threshold":8,"value":4,"declarationName":"Foo.add","declarationType":"method"}]}],
"analyzeResults":[{"path":"` + file + `","issues":[{"id":"avoid-dynamic","message":"Avoid using dynamic type.","location":{"startLine":1}}]}]}`
}

func TestImportDcmMeasuresAndCaps(t *testing.T) {
	bin := ratchet(t)
	dir := t.TempDir()
	cmdtest.WriteFile(t, dir, "calm.json", dcmReport("lib/a.dart", 1))
	cmdtest.WriteFile(t, dir, "spiky.json", dcmReport("lib/a.dart", 30))
	cmdtest.WriteFile(t, dir, "other.json", dcmReport("lib/b.dart", 7))
	cmdtest.WriteFile(t, dir, "empty.json", `{"formatVersion":13,"metricResults":[]}`)

	r := bin.Run(t, dir, "import", "calm.json", "--root", ".").MustPass(t)
	r.MustSay(t, "measured 1 declaration", "not imported: 1 DCM lint issue", "is under --root")
	r.MustNotSay(t, "imported 0 findings")
	bin.Run(t, dir, "import", "calm.json", "--root", ".", "--json").MustPass(t).
		MustSay(t, `"name": "Foo.add"`, `"cyclomatic": 1`, `"files": 1`)
	bin.Run(t, dir, "import", "empty.json", "--root", ".").MustFail(t).MustSay(t, "dcm: metrics:")

	m := bin.Run(t, dir, "import", "calm.json", "--root", ".", "--emit", "measures",
		"--repo", "app", "--ts", "2026-01-15", "--commit", "abc123", "--org=-").MustPass(t)
	m.MustSay(t, `"scope":"function"`, `"path":"lib/a.dart:Foo.add"`, `"metric":"cyclomatic"`, `"metric":"widgets.nesting"`,
		`"metric":"cyclomatic.p90"`, `"metric":"funcs","value":1`, `"repo":"app"`, `"commit":"abc123"`, `"ts":"2026-01-15T00:00:00Z"`)
	m.MustNotSay(t, `"metric":"findings.`) // DCM carries no findings source; a zero would blank a real lint series
	d := bin.Run(t, dir, "import", "calm.json", "--root", ".", "--emit", "measures", "--detail=false", "--org=-").MustPass(t)
	d.MustSay(t, `"metric":"cyclomatic.max"`)
	d.MustNotSay(t, `"scope":"function"`)

	// Caps come from the metrics, and a check that could not fail is refused rather than passed.
	bin.Run(t, dir, "import", "calm.json", "--root", ".", "--mode", "baseline").MustPass(t)
	bin.Run(t, dir, "import", "calm.json", "--root", ".", "--mode", "check").MustFail(t).MustSay(t, "nothing to gate")
	bin.Run(t, dir, "import", "calm.json", "--root", ".", "--mode", "check", "--strict-caps").MustPass(t).MustSay(t, "0 new")
	bin.Run(t, dir, "import", "spiky.json", "--root", ".", "--mode", "check", "--strict-caps").MustFail(t).
		MustSay(t, "cap breach", "cyclomatic 30 exceeds baseline 1")

	// Several reports union into one import; the same report twice is not twice the code.
	bin.Run(t, dir, "import", "calm.json", "other.json", "--root", ".", "--emit", "measures", "--repo", "app", "--org=-").MustPass(t).
		MustSay(t, "merged 2 reports", `"path":"lib/b.dart:Foo.add"`, `"metric":"funcs","value":2`)
	bin.Run(t, dir, "import", "calm.json", "calm.json", "--root", ".", "--emit", "measures", "--repo", "app", "--org=-").MustPass(t).
		MustSay(t, `"metric":"funcs","value":1`)
	bin.Run(t, dir, "import", "calm.json", "spiky.json", "--root", ".").MustFail(t).MustSay(t, "--prefix")

	// --prefix keys a per-package report like the rest of the repository, and report=prefix pairs
	// let one invocation import every package and roll the project series up once.
	bin.Run(t, dir, "import", "calm.json", "--root", ".", "--prefix", "packages/app", "--json").MustPass(t).
		MustSay(t, `"file": "packages/app/lib/a.dart"`)
	bin.Run(t, dir, "import", "calm.json=packages/app", "other.json=packages/lib", "--root", ".",
		"--emit", "measures", "--repo", "app", "--org=-").MustPass(t).
		MustSay(t, `"path":"packages/app/lib/a.dart:Foo.add"`, `"path":"packages/lib/lib/b.dart:Foo.add"`, `"metric":"funcs","value":2`)
	cmdtest.WriteFile(t, dir, "r.sarif", `{"version":"2.1.0","runs":[]}`)
	bin.Run(t, dir, "import", "r.sarif=packages/app", "--root", ".").MustFail(t).MustSay(t, "=prefix applies to DCM")
}
