package arch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/sherzing/assay/internal/model"
)

// Rule is the rule id carried on every finding this package emits.
const Rule = "layer-violation"

// Violation is one package reaching a layer it may not reach.
type Violation struct {
	Forbid  Forbid
	SrcPkg  string
	DstPkg  string
	Chain   []string
	SrcFile string
}

// Check evaluates every forbid against the graph.
//
// Reports one violation per (source package, destination package) pair rather
// than per chain. A package that reaches infrastructure by four routes has one
// architectural problem, not four, and four findings would make the fix look
// bigger than it is.
func Check(d *Decl, g *Graph) []Violation {
	var out []Violation

	for _, f := range d.Forbids {
		for _, pkg := range g.Packages() {
			layer, ok := d.LayerOf(pkg)
			if !ok || layer != f.From {
				continue
			}
			inTarget := func(p string) bool {
				l, ok := d.LayerOf(p)
				return ok && l == f.To
			}
			chain := g.Reachable(pkg, inTarget)
			if chain == nil {
				continue
			}
			out = append(out, Violation{
				Forbid:  f,
				SrcPkg:  pkg,
				DstPkg:  chain[len(chain)-1],
				Chain:   chain,
				SrcFile: g.Files[pkg],
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Forbid.String() != out[j].Forbid.String() {
			return out[i].Forbid.String() < out[j].Forbid.String()
		}
		if out[i].SrcPkg != out[j].SrcPkg {
			return out[i].SrcPkg < out[j].SrcPkg
		}
		return out[i].DstPkg < out[j].DstPkg
	})
	return out
}

// Fingerprint identifies a violation across refactors.
//
// Deliberately excludes the CHAIN and any line number. Extracting an unrelated
// helper lengthens the chain without changing the architectural fact, and if
// the fingerprint moved, a tolerated violation would reappear as a new one and
// the ratchet would cry wolf. What identifies the violation is which layer
// reaches which, from which package to which.
func (v Violation) Fingerprint() string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		Rule, v.Forbid.From, v.Forbid.To, v.SrcPkg, v.DstPkg,
	}, "\x00")))
	return hex.EncodeToString(h[:8])
}

// Message renders the chain, which is the actionable part.
func (v Violation) Message() string {
	if len(v.Chain) == 2 {
		return fmt.Sprintf("%s must not reach %s: %s imports %s",
			v.Forbid.From, v.Forbid.To, v.SrcPkg, v.DstPkg)
	}
	return fmt.Sprintf("%s must not reach %s: %s",
		v.Forbid.From, v.Forbid.To, strings.Join(v.Chain, " → "))
}

// Suggest names the hop to cut. For an indirect chain the useful advice is
// almost never "delete the import" — it is "the intermediate package is doing
// two jobs".
func (v Violation) Suggest() string {
	if len(v.Chain) == 2 {
		return "invert the dependency: have " + v.DstPkg + " depend on " + v.SrcPkg + ", or move the shared type into " + v.Forbid.From
	}
	return "the indirection through " + v.Chain[1] + " does not change the dependency; split " + v.Chain[1] + " so the part " + v.SrcPkg + " needs does not reach " + v.DstPkg
}

// Report converts violations into the shared finding model, so plumb composes
// with the ratchet, the store and the ticketing tool without any of them
// knowing what an architecture is.
//
// The graph is nil when the declaration has no forbid rules — an ownership-only
// declaration on a codebase that is not a Go module still produces a report.
func Report(d *Decl, g *Graph, vs []Violation, drifts []Drift) *model.Report {
	rep := &model.Report{Findings: []model.Finding{}}
	pkgs := 0
	if g != nil {
		rep.Root, pkgs = g.Module, len(g.Edges)
	}
	for _, v := range vs {
		file := v.SrcFile
		if file == "" {
			file = v.SrcPkg
		}
		rep.Findings = append(rep.Findings, model.Finding{
			Rule:     Rule,
			Severity: model.Error,
			File:     file,
			// The declaration's SHA travels with the finding, so a record says
			// which version of the rules produced it.
			Func:        d.SHA,
			Message:     v.Message(),
			Suggest:     v.Suggest(),
			Fingerprint: v.Fingerprint(),
		})
	}
	rep.Findings = append(rep.Findings, driftFindings(d, drifts)...)
	rep.Summarise(pkgs)
	return rep
}

// DeadLayers names declared paths that match no package.
//
// The rule id stays "layer-violation": it is part of every fingerprint in
// every committed baseline, and renaming it would make all of them reappear
// as new.
//
// A rule guarding a directory that no longer exists is a rule everyone believes
// is protecting them. Worth a warning, not a failure: the directory may be
// about to be created.
//
// Reported per PATH, not per layer. A layer with three paths of which one is a
// typo still matches packages, so a layer-level check stays silent while a
// third of the rule quietly protects nothing — which is exactly what happened
// to this project's own declaration the first time it ran.
func DeadLayers(d *Decl, g *Graph) []string {
	pkgs := g.Packages()
	var dead []string
	for _, name := range d.Order {
		for _, p := range d.Layers[name] {
			matched := false
			for _, pkg := range pkgs {
				if pkg == p || strings.HasPrefix(pkg, p+"/") {
					matched = true
					break
				}
			}
			if !matched {
				dead = append(dead, name+" ("+p+")")
			}
		}
	}
	return dead
}
