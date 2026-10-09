// Package arch checks a codebase against an architecture a human declared,
// rather than trying to discover bad architecture on its own.
//
// THE DECLARATION LIVES IN THE DOCUMENT. Prose in one file and linter config in
// another will drift, and the prose is the half that loses — it becomes
// aspirational while the config becomes the truth, and nobody notices for a
// year. A fenced block inside ARCHITECTURE.md cannot drift from the prose
// around it, because they are the same file and the same review.
//
// It also means an agent reads the artefact it is bound by. A CLAUDE.md that
// describes the architecture is a prompt; a block that CI enforces is a gate.
//
// WHY TRANSITIVE. A direct-import check is not enough, and the gap is not
// theoretical: with a rule forbidding domain -> infra, a direct import is
// caught and `domain -> helper -> infra` is silent. Same architectural
// dependency, one hop. No workaround is required to produce it — an ordinary
// "extract a helper" refactor does. See Reachable.
package arch

import (
	"fmt"
	"regexp"
	"strings"
)

// Decl is a parsed architecture declaration.
type Decl struct {
	// Layers maps a group name to the module-relative path prefixes in it.
	// The keyword is `group`; `layer` is accepted as a synonym. The field keeps
	// its old name because renaming it buys nothing a reader of the document
	// would see.
	Layers map[string][]string
	// Order is declaration order, so output does not depend on map iteration.
	Order []string
	// Forbids are the dependency rules.
	Forbids []Forbid
	// Owns maps a layer to the vocabulary it owns. A layer absent from this map
	// is a consumer and is never checked for drift. See lexicon.go.
	Owns map[string][]string
	// OwnsOrder is declaration order of the owning layers.
	OwnsOrder []string
	// SHA identifies the version of the declaration that produced a finding.
	// Empty outside a git work tree.
	SHA string
}

// Forbid states that no package in From may reach any package in To.
type Forbid struct {
	From, To string
	Line     int
}

func (f Forbid) String() string { return f.From + " -> " + f.To }

// blockRe finds the fenced arch block. Non-greedy so the first block wins and a
// later ```sh example cannot swallow the document.
var blockRe = regexp.MustCompile("(?s)```arch\\s*\n(.*?)```")

var forbidRe = regexp.MustCompile(`^forbid\s+(\S+)\s*->\s*(\S+)\s*$`)

// ParseDoc extracts and parses the fenced arch block from a markdown document.
//
// A document with no block is an ERROR rather than a pass. Returning "no
// violations" for a file that enforces nothing is the worst available
// behaviour: CI goes green and everyone believes the architecture is checked.
func ParseDoc(doc string) (*Decl, error) {
	m := blockRe.FindStringSubmatchIndex(doc)
	if m == nil {
		return nil, fmt.Errorf("no ```arch block found: the document declares nothing, so nothing can be enforced")
	}
	// Line number of the block's first content line, for error messages that
	// point into the markdown rather than into an extracted fragment.
	offset := strings.Count(doc[:m[2]], "\n") + 1
	return Parse(doc[m[2]:m[3]], offset)
}

// Parse reads the body of an arch block. lineBase is the line number in the
// enclosing document at which the body starts.
func Parse(body string, lineBase int) (*Decl, error) {
	d := &Decl{Layers: map[string][]string{}, Owns: map[string][]string{}}
	type ownsStmt struct {
		layer string
		terms []string
		line  int
	}
	var owns []ownsStmt

	for i, raw := range strings.Split(body, "\n") {
		line := raw
		if h := strings.IndexByte(line, '#'); h >= 0 {
			line = line[:h]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		n := lineBase + i

		switch {
		case strings.HasPrefix(line, "group "), strings.HasPrefix(line, "layer "):
			f := strings.Fields(line)
			if len(f) < 3 {
				return nil, fmt.Errorf("line %d: group needs a name and at least one path: %q", n, line)
			}
			name := f[1]
			if _, dup := d.Layers[name]; dup {
				return nil, fmt.Errorf("line %d: group %q declared twice", n, name)
			}
			paths := make([]string, 0, len(f)-2)
			for _, p := range f[2:] {
				paths = append(paths, strings.Trim(p, "/"))
			}
			d.Layers[name] = paths
			d.Order = append(d.Order, name)

		case strings.HasPrefix(line, "forbid"):
			g := forbidRe.FindStringSubmatch(line)
			if g == nil {
				return nil, fmt.Errorf("line %d: expected `forbid <group> -> <group>`, got %q", n, line)
			}
			d.Forbids = append(d.Forbids, Forbid{From: g[1], To: g[2], Line: n})

		case strings.HasPrefix(line, "owns "):
			f := strings.Fields(line)
			if len(f) < 3 {
				return nil, fmt.Errorf("line %d: owns needs a group and at least one term: %q", n, line)
			}
			terms := make([]string, 0, len(f)-2)
			for _, t := range f[2:] {
				terms = append(terms, strings.ToLower(t))
			}
			owns = append(owns, ownsStmt{layer: f[1], terms: terms, line: n})

		default:
			// Not ignored. A misspelled `forbidd` silently dropping a
			// constraint is precisely how a gate stops meaning anything.
			return nil, fmt.Errorf("line %d: unknown statement %q (expected `group`, `forbid` or `owns`; `layer` is accepted as a synonym of `group`)", n, strings.Fields(line)[0])
		}
	}

	// A term has exactly one owner. Two layers claiming "rating" is not a
	// shared vocabulary, it is the boundary question left unanswered.
	ownerOf := map[string]string{}
	for _, o := range owns {
		if _, ok := d.Layers[o.layer]; !ok {
			return nil, fmt.Errorf("line %d: owns references undeclared group %q", o.line, o.layer)
		}
		if _, seen := d.Owns[o.layer]; !seen {
			d.OwnsOrder = append(d.OwnsOrder, o.layer)
		}
		for _, t := range o.terms {
			if prev, dup := ownerOf[t]; dup && prev != o.layer {
				return nil, fmt.Errorf("line %d: term %q is already owned by %q — a term has one owner", o.line, t, prev)
			}
			if ownerOf[t] == o.layer {
				continue
			}
			ownerOf[t] = o.layer
			d.Owns[o.layer] = append(d.Owns[o.layer], t)
		}
	}

	if len(d.Forbids) == 0 && len(d.Owns) == 0 {
		return nil, fmt.Errorf("the arch block declares no `forbid` or `owns` rules, so it enforces nothing")
	}
	for _, f := range d.Forbids {
		if _, ok := d.Layers[f.From]; !ok {
			return nil, fmt.Errorf("line %d: forbid references undeclared group %q", f.Line, f.From)
		}
		if _, ok := d.Layers[f.To]; !ok {
			return nil, fmt.Errorf("line %d: forbid references undeclared group %q", f.Line, f.To)
		}
		if f.From == f.To {
			return nil, fmt.Errorf("line %d: group %q cannot be forbidden from itself", f.Line, f.From)
		}
	}
	return d, nil
}

// LayerOf returns the declared layer containing a module-relative package path,
// and whether one matched.
//
// Longest prefix wins, so `internal/domain/billing` in its own layer beats
// `internal/domain` in another. Without that rule the answer would depend on
// declaration order, which is a surprising thing for an architecture to hinge
// on.
func (d *Decl) LayerOf(rel string) (string, bool) {
	best, bestLen := "", -1
	for _, name := range d.Order {
		for _, p := range d.Layers[name] {
			if rel == p || strings.HasPrefix(rel, p+"/") {
				if len(p) > bestLen {
					best, bestLen = name, len(p)
				}
			}
		}
	}
	return best, bestLen >= 0
}
