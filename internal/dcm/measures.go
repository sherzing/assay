package dcm

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sherzing/assay/internal/model"
	"github.com/sherzing/assay/pkg/schema"
)

// names maps DCM metric IDs to the names the Go scan emits, so one lens query serves both.
var names = map[string]string{
	"cyclomatic-complexity":            "cyclomatic",
	"maximum-nesting-level":            "nesting",
	"number-of-parameters":             "params",
	"source-lines-of-code":             "sloc",
	"lines-of-code":                    "loc",
	"halstead-volume":                  "halstead.volume",
	"maintainability-index":            "maintainability",
	"number-of-used-widgets":           "widgets.used",
	"widgets-nesting-level":            "widgets.nesting",
	"number-of-methods":                "methods",
	"number-of-added-methods":          "methods.added",
	"number-of-overridden-methods":     "methods.overridden",
	"number-of-implemented-interfaces": "interfaces",
	"depth-of-inheritance-tree":        "inheritance.depth",
	"coupling-between-object-classes":  "coupling",
	"response-for-class":               "rfc",
	"tight-class-cohesion":             "cohesion",
	"weight-of-class":                  "weight",
	"weighted-methods-per-class":       "wmc",
	"number-of-imports":                "imports",
	"number-of-external-imports":       "imports.external",
	"technical-debt":                   "technical.debt",
}

// classTypes are the declaration types that land at class scope; classLevel is the fallback
// by metric ID for a report that carries no declarationType.
var classTypes = map[string]bool{"class": true, "mixin": true, "extension": true, "extension type": true, "enum": true}

var classLevel = map[string]bool{
	"number-of-methods": true, "number-of-added-methods": true, "number-of-overridden-methods": true,
	"number-of-implemented-interfaces": true, "depth-of-inheritance-tree": true,
	"coupling-between-object-classes": true, "response-for-class": true, "tight-class-cohesion": true,
	"weight-of-class": true, "weighted-methods-per-class": true,
}

var fileScoped = map[string]bool{
	"number-of-imports": true, "number-of-external-imports": true, "technical-debt": true,
}

var (
	generatedFileRe   = regexp.MustCompile(`\.(g|freezed|gr|mocks|pb|pbenum|pbjson|pbserver|pbgrpc)\.dart$`)
	generatedHeaderRe = regexp.MustCompile(`(?im)^\s*//.*\bgenerated\b.*\bdo not (edit|modify)\b`)
)

func (im *importer) values(files []fileIssues) {
	for _, f := range files {
		file := im.rebase(f.path)
		if im.generated(file) {
			im.res.Generated++
			continue
		}
		im.files[file] = true
		ids := identities(f.issues)
		for _, is := range f.issues {
			im.value(file, is, ids[declKey(is)])
		}
	}
}

// rebase puts a report path under Prefix, so a per-package report keys like the rest of the repository.
func (im *importer) rebase(p string) string {
	if im.opt.Prefix == "" {
		return p
	}
	return path.Join(filepath.ToSlash(im.opt.Prefix), p)
}

// generated applies the Dart conventions for code a tool wrote: the file name, a generated/
// directory, or a header saying so, read under Root when the file is there. The Go scan skips
// generated code too: measuring it tells us about a generator.
func (im *importer) generated(p string) bool {
	if generatedFileRe.MatchString(p) {
		return true
	}
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "generated" {
			return true
		}
	}
	src, ok := im.read(p)
	return ok && generatedHeaderRe.Match(src)
}

// read returns the head of a report file when it is under Root, and counts it as located.
func (im *importer) read(p string) ([]byte, bool) {
	if im.opt.Root == "" {
		return nil, false
	}
	src, err := os.ReadFile(filepath.Join(im.opt.Root, filepath.FromSlash(p)))
	if err != nil {
		return nil, false
	}
	im.res.Located++
	if len(src) > 4096 {
		src = src[:4096]
	}
	return src, true
}

// value records one measurement at its scope; an identity and metric seen twice is kept once.
func (im *importer) value(file string, is issue, id string) {
	scope, mpath := scopeOf(is, file, id)
	v, ok := num(is.Value)
	if !ok {
		return
	}
	name := nameOf(is.ID)
	key := string(scope) + "\x00" + mpath + "\x00" + name
	if im.seen[key] {
		return
	}
	im.seen[key] = true
	im.res.Measures = append(im.res.Measures, schema.Measure{Scope: scope, Path: mpath, Metric: name, Value: v})
	if scope == schema.ScopeFunction {
		im.keep(id, file, is, name, v)
	}
}

// keep fills the per-function record in the Go scan's shape, so a Dart baseline gets real caps.
// sloc is not a statement count, so Statements stays 0.
func (im *importer) keep(id, file string, is issue, name string, v float64) {
	e, ok := im.fm[file+":"+id]
	if !ok {
		e = &entry{f: model.FuncMetrics{Name: id, File: file, Line: is.Location.StartLine, Exported: exported(id)}}
		im.fm[file+":"+id] = e
	}
	switch name {
	case "cyclomatic":
		e.f.Cyclomatic, e.cyclomatic = int(v), true
	case "nesting":
		e.f.MaxNesting, e.nesting, im.nesting = int(v), true, true
	case "params":
		e.f.Params = int(v)
	}
}

func exported(id string) bool {
	for _, seg := range strings.Split(id, ".") {
		if strings.HasPrefix(seg, "_") {
			return false
		}
	}
	return true
}

func nameOf(id string) string {
	if name, ok := names[id]; ok {
		return name
	}
	return strings.ReplaceAll(id, "-", ".")
}

// scopeOf places a metric at the level it measures.
func scopeOf(is issue, file, id string) (schema.Scope, string) {
	switch {
	case is.DeclarationName == "" || fileScoped[is.ID]:
		return schema.ScopeFile, file
	case classTypes[is.DeclarationType], is.DeclarationType == "" && classLevel[is.ID]:
		return schema.ScopeClass, file + ":" + id
	}
	return schema.ScopeFunction, file + ":" + id
}

// fileOf is the file part of a measure path.
func fileOf(p string) string {
	if i := strings.Index(p, ":"); i >= 0 {
		return p[:i]
	}
	return p
}

// decl is one declaration DCM measured, as its metric entries describe it.
type decl struct {
	name, typ  string
	start, end int
	id         string
}

func declKey(is issue) string {
	return is.DeclarationName + "\x00" + is.DeclarationType + "\x00" + strconv.Itoa(is.Location.StartLine)
}

// identities names every declaration in a file so that none of DCM's repeated names collide:
// a setter is `name=` as Dart writes it, a local function or an unnamed extension's member is
// qualified by what encloses it, and whatever still collides gets an ordinal in file order.
func identities(issues []issue) map[string]string {
	byKey, ds := declarations(issues)
	used := map[string]int{}
	for i, d := range ds {
		id := d.name
		if d.typ == "setter" {
			id += "="
		}
		if c := enclosing(ds[:i], d); c != nil && !strings.HasPrefix(id, c.id+".") {
			id = c.id + "." + id
		}
		used[id]++
		if n := used[id]; n > 1 {
			id = fmt.Sprintf("%s#%d", id, n)
		}
		d.id = id
	}
	out := make(map[string]string, len(byKey))
	for k, d := range byKey {
		out[k] = d.id
	}
	return out
}

// declarations collects the distinct declarations a file's metric entries describe, outer first.
func declarations(issues []issue) (map[string]*decl, []*decl) {
	var ds []*decl
	byKey := map[string]*decl{}
	for _, is := range issues {
		if is.DeclarationName == "" {
			continue
		}
		k := declKey(is)
		d, ok := byKey[k]
		if !ok {
			d = &decl{name: is.DeclarationName, typ: is.DeclarationType, start: is.Location.StartLine, end: is.Location.EndLine}
			byKey[k] = d
			ds = append(ds, d)
		}
		d.end = max(d.end, is.Location.EndLine)
	}
	sort.SliceStable(ds, func(i, j int) bool {
		if ds[i].start != ds[j].start {
			return ds[i].start < ds[j].start
		}
		return ds[i].end > ds[j].end
	})
	return byKey, ds
}

// enclosing is the innermost declaration whose span contains d. Candidates are sorted by start,
// so the last match is the innermost.
func enclosing(before []*decl, d *decl) *decl {
	var c *decl
	for _, b := range before {
		if b.start <= d.start && d.end <= b.end && (b.start < d.start || b.end > d.end) {
			c = b
		}
	}
	return c
}
