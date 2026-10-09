package arch

import (
	"fmt"
	"sort"
)

// Change classifies a declaration diff.
type Change int

const (
	// NoChange means the two declarations are semantically identical.
	// Reordering statements is not a change.
	NoChange Change = iota
	// Tightening adds a rule or narrows a group. Strictly more is forbidden.
	Tightening
	// Loosening removes a rule or widens a group. Strictly less is forbidden.
	Loosening
	// Mixed both tightens and loosens.
	Mixed
)

func (c Change) String() string {
	switch c {
	case NoChange:
		return "no change"
	case Tightening:
		return "tightening"
	case Loosening:
		return "loosening"
	default:
		return "mixed"
	}
}

// NeedsReview reports whether a change weakens the architecture.
//
// THE ASYMMETRY IS THE POINT. A change process everyone routes around is worse
// than none, so tightening must be free — the common case, and the one nobody
// needs protecting from. Only loosening costs a second reviewer.
func (c Change) NeedsReview() bool { return c == Loosening || c == Mixed }

// Diff classifies the move from old to new.
func Diff(old, new *Decl) (Change, []string) {
	var added, removed []string

	oldF, newF := forbidSet(old), forbidSet(new)
	for f := range newF {
		if !oldF[f] {
			added = append(added, "+ forbid "+f)
		}
	}
	for f := range oldF {
		if !newF[f] {
			removed = append(removed, "- forbid "+f)
		}
	}

	// A group that grows brings more packages under its rules; one that shrinks
	// releases packages from them. Same logic, one level down.
	for _, name := range union(old.Order, new.Order) {
		o, inOld := old.Layers[name]
		n, inNew := new.Layers[name]
		switch {
		case inOld && !inNew:
			removed = append(removed, "- group "+name)
			continue
		case !inOld && inNew:
			added = append(added, "+ group "+name)
			continue
		}
		os, ns := set(o), set(n)
		for p := range ns {
			if !os[p] {
				added = append(added, fmt.Sprintf("+ %s covers %s", name, p))
			}
		}
		for p := range os {
			if !ns[p] {
				removed = append(removed, fmt.Sprintf("- %s no longer covers %s", name, p))
			}
		}
	}

	// Ownership: a term added is one more thing that can drift, so it tightens;
	// a term removed releases every declaration that carried it.
	for _, name := range union(old.OwnsOrder, new.OwnsOrder) {
		os, ns := set(old.Owns[name]), set(new.Owns[name])
		for t := range ns {
			if !os[t] {
				added = append(added, fmt.Sprintf("+ %s owns %s", name, t))
			}
		}
		for t := range os {
			if !ns[t] {
				removed = append(removed, fmt.Sprintf("- %s no longer owns %s", name, t))
			}
		}
	}

	sort.Strings(added)
	sort.Strings(removed)
	detail := append(added, removed...)

	switch {
	case len(added) == 0 && len(removed) == 0:
		return NoChange, nil
	case len(removed) == 0:
		return Tightening, detail
	case len(added) == 0:
		return Loosening, detail
	default:
		return Mixed, detail
	}
}

func forbidSet(d *Decl) map[string]bool {
	out := map[string]bool{}
	for _, f := range d.Forbids {
		out[f.String()] = true
	}
	return out
}

func set(xs []string) map[string]bool {
	out := map[string]bool{}
	for _, x := range xs {
		out[x] = true
	}
	return out
}

func union(a, b []string) []string {
	seen, out := map[string]bool{}, []string{}
	for _, xs := range [][]string{a, b} {
		for _, x := range xs {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Accepted reports whether a classification may pass: anything that is not a
// weakening, or a weakening that arrived with a new Why entry.
func Accepted(c Change, explained []string) bool {
	return !c.NeedsReview() || len(explained) > 0
}
