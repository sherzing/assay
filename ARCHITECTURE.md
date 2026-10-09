# Architecture

assay is a set of small tools over a shared data format. The structure follows
from that: the format is the only thing every tool agrees on, so it must not
depend on any of them.

Four rules, in order of how much they would cost to get wrong.

**The schema depends on nothing.** `pkg/schema` is the published contract. It is
the one package another organisation's tool might import, and a tool in another
language must be able to reimplement it from the record types alone. If it
reached into `internal/`, the contract would quietly acquire our implementation.

**The tools do not import each other.** `ratchet`, `strata`, `lens`, `docket`,
`plumb` and `judge` compose over JSONL, not over Go symbols. A dependency
between two of them would make the composability claim false in the one place
it is easiest to check, and would mean installing one pulled in another.

**Analysis does not depend on presentation, storage, gating or ticketing.**
`internal/analyze`, `internal/arch`, `internal/learn` and `internal/judge`
measure and judge; they produce findings and nothing else. If measurement
imported rendering, a change to output formatting could alter a number. If it
imported the store, a scan would need a store to exist. If it imported the
baseline, "what is wrong" and "what we tolerate" would blur into one function.

**Each group declares its own concepts.** The rule above says who may *reach*
whom. It cannot say whether a declaration *belongs*: a rollup function inside
the baseline package is perfectly layered and still in the wrong place. So each
group below names the vocabulary it owns. A type or function whose name carries
another group's vocabulary is responsibility drift, wherever its imports point.

## How the tools fit together

The interface between tools is not a Go API. It is four record types, one JSON
object per line, defined in `pkg/schema` and readable with `grep` and `jq`
without any binary here. A tool that reads and writes them composes with the
rest, in any language.

| record | produced by | consumed by |
|---|---|---|
| `finding` | `ratchet scan`, `ratchet import` (SARIF from any linter), `plumb`, `judge` | `ratchet check`, `strata append`, `docket`, `lens` |
| `measure` | `ratchet scan`, `ratchet history` | `strata append`, `strata rollup`, `lens` |
| `verdict` | `ratchet scan --emit` (from `quality:` comments and `.quality.yaml`), `strata verdicts` | `strata precision`, `strata export`, `docket` |
| `ticket` | `docket create`, `docket sync` | `docket status`, `docket sync` |

Two files are contracts of a different kind. `.ratchet-baseline.json` and
`.plumb-baseline.json` record what a repository has agreed to tolerate; they
are committed, and the gate fails only on what is new. `ARCHITECTURE.md` is
this file: the block below is what `plumb` enforces and `judge` reads for
intent. How each tool is run, and which rules `ratchet` ships with, is in the
README and `docs/USAGE.md` — that is usage, and it changes more often than
structure.

## Reading the enforced block

The block is a fenced ` ```arch ` section. Blank lines and anything after `#`
are ignored. Every other line is one of three statements, and an unknown word
is an error rather than a no-op, so a typo cannot silently drop a rule.

| statement | meaning |
|---|---|
| `group <name> <path>...` | Names a group and the module-relative directories in it. `layer` is accepted as a synonym; it means nothing more than `group`. A package belongs to the group with the longest matching prefix, so `internal/domain/billing` in its own group beats `internal/domain` in another. |
| `forbid <a> -> <b>` | No package in group `a` may reach any package in group `b`, **transitively**. A direct-import check would miss `a -> helper -> b`, which is the same dependency one hop away and is what an ordinary refactor produces. Go only. |
| `owns <group> <term>...` | The vocabulary this group declares. A type or function in this group whose name carries a term another group owns is `responsibility-drift`. Calls are never checked; that is what `forbid` is for. A term has exactly one owner. A group with no `owns` line is a consumer and is never checked. Any language `plumb` can read. |

Both rules are checked against the code by `plumb`, tolerated by
`.plumb-baseline.json`, and classified on change by `plumb diff`: adding a
rule or a term is a tightening and is free; removing one is a loosening and
needs a second reviewer and an entry under Why. The entry is what `plumb diff`
checks: a paragraph under `## Why` starting with a bold date, `**2026-01-31.**`,
that the previous version did not have. The reviewer is the pull request review.

<!-- The block below is ENFORCED by `make arch`. It is not decoration.
     Editing it is an architectural change: `plumb diff` classifies it, and a
     loosening needs a second reviewer and an entry under Why. -->

```arch
group schema      pkg/schema
group model       internal/model
group analysis    internal/analyze internal/arch internal/learn internal/judge
group verdicts    internal/verdict
group history     internal/store internal/evidence
group gate        internal/baseline
group tickets     internal/docket
group import      internal/sarif
group presenting  internal/report

# The schema depends on nothing.
forbid schema    -> model
forbid schema    -> analysis
forbid schema    -> verdicts
forbid schema    -> history
forbid schema    -> gate
forbid schema    -> tickets
forbid schema    -> import
forbid schema    -> presenting

# The finding model is shared by everyone and knows nobody.
forbid model     -> analysis
forbid model     -> history
forbid model     -> presenting

# Analysis produces findings and nothing else.
forbid analysis  -> presenting
forbid analysis  -> history
forbid analysis  -> gate
forbid analysis  -> tickets

# Neither the store nor the tracker knows how a finding was produced or shown.
forbid history   -> analysis
forbid history   -> presenting
forbid tickets   -> analysis
forbid tickets   -> history
forbid presenting -> analysis
forbid presenting -> history

# What each group may declare. A term has one owner; a group with no line
# is a consumer and may name anything.
owns schema      encoder decoder kind judgement
owns model       metric summary dist cognitive cyclomatic nesting
owns analysis    scan smell probe layer graph violation drift excerpt answer stem candidate
owns verdicts    org comment expired glob resolve
owns history     store rollup bucket append precision quarter corpus assessment
owns gate        baseline tolerated tighten regressed cap
owns tickets     plan cohort linear sync progress close
owns import      import
owns presenting  text json csv render format
```

## Why

**2026-09-20.** Written when `plumb` was built, from three rules the codebase
already followed — so this records an existing shape rather than imposing a new
one. A declaration that fails on the day it is written teaches everyone to
ignore it.

**2026-09-22.** Split the former `internals` group into `model`, `verdicts`,
`history`, `gate`, `tickets` and `import`, and added ownership. The old group
was a bucket: six packages with six different jobs, and a rule that only said
the schema must not reach any of them. Every `forbid` above was checked against
the import graph before it was written, so this is still a record of the shape
the code has, only at a resolution where ownership means something.

**2026-10-09.** The keyword is `group`; `layer` remains a synonym. The checks never
assumed a style, but the word did: it reads as clean architecture to anyone
who has heard of it, and the README example was a textbook onion. A group is a
named set of directories and nothing more. Two groups and one rule is a
complete declaration. No rule here changed.

Terms deliberately left unowned, because two groups legitimately declare them:
`finding` and `severity` (the wire record in `schema` and the internal model in
`model`), `verdict` and `ticket` (the record and the package that interprets
it), `sarif` (read by `import`, written by `presenting`), `measure` (a record,
and what the store queries). Owning any of these would make the rule cry wolf
on the first scan, which is the fastest way to get it switched off.

The tools-do-not-import-each-other rule is not expressible here: `plumb`
covers package dependencies, and `cmd/*` are separate mains that the import
graph already keeps apart. It is checked by the fact that each binary builds
alone. If that ever stops being true, add a group per command.
