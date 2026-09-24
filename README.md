# assay

Small, independent tools for measuring code quality over time. They compose over
JSON Lines, following the Unix model: each does one thing, reads a stream, writes
a stream.

```sh
ratchet scan . --emit measures | strata append --repo myservice
golangci-lint run --out-format sarif | ratchet import - | ratchet check
strata query --repo myservice --metric cognitive.p90 --since 2026-01 --format csv
```

## Why

Most of a code-quality product is *platform* — server, storage, ingestion,
dashboards — and AI made that kind of platform cheap to build. It did not make
**good rules** cheap: authoring a syntactically valid rule takes seconds, knowing
whether it is mostly noise still takes judgement and real codebases.

So the value is not the tool. It is the **rule corpus and the precision data
behind it** — the part that compounds, because it is accumulated judgement.

## The contract is the format

Composability comes from a shared data format, not from good module boundaries.
`ls | grep | wc` works because of text. Four versioned JSONL records:

| record | what it says |
|---|---|
| `finding` | something is wrong at this location |
| `measure` | this number, this commit, this scope (project/module/file/function/class) |
| `verdict` | a human judged this finding |
| `ticket` | this cohort of findings is tracked as one issue |

The record types in `pkg/schema` are the specification, and the JSONL they
produce is the contract — a conforming tool in any language composes with these.
A golden test pins the exact wire bytes of every record, so a field cannot be
renamed without breaking a build.

## Tools

| | |
|---|---|
| **`ratchet`** | the gate. `scan`, `baseline`, `check`, `import` (SARIF from any linter, DCM metrics for Dart), `history`, `exceptions`, `learn` |
| **`strata`** | append-only history. `append`, `query`, `rollup`, `verdicts`, `stat`, `precision`, `export`, `verify`, `promote-check` |
| **`lens`** | read a stream at a glance. `top`, `trend`, `diff`, `compare`, `calibrate` |
| **`docket`** | turn findings into tickets. `plan`, `create`, `sync`, `status` |
| **`plumb`** | verify dependencies and ownership against the declaration. `scan`, `check`, `baseline`, `diff`, `learn` |
| **`judge`** | ask a model whether declarations belong where they are, cited against the document. `scan`, `doctor` |

Each is usable with the others absent. `strata` has nothing quality-specific in
it — point it at any conforming stream and range queries come back.

There is deliberately **no web UI**. `lens` covers reading the data from a
terminal; Grafana over the rollups or a static site from JSONL covers the rest.

```
$ lens trend --metric cplx.per_kloc --period month
service-c     ▅▇▇█▇▇▆▇▇▇▆▆▆▆▆▅▅▄▄▄▄▄▄▄▃▃▂▂▃▂▁▂▁▁▁▁▁▁▁▁▂▁▂▂▂▂▁▁▁▂▁▂▂▁▂▂▂▃▄▄▅▅▅  49.37 → 49.00
               2021-05-01 → 2026-07-01  (63 points)
```

**Full guide: [docs/USAGE.md](docs/USAGE.md)** — CI gating, SARIF from other
languages, where rules live, writing rules that are worth having, keeping
history, and which external tools to reach for.

## The verdict split is the point

A single "tolerated" bucket conflates two different things:

| verdict | is it debt? | feeds rule quality? |
|---|---|---|
| `accepted` | yes — pay it down | no |
| `false-positive` | no — the rule is wrong | **yes** |
| `wont-fix` | no | no |

False-positive rate per rule becomes a **rule quality metric**. Rank rules by
measured precision, retire the noisy ones, publish the data. That is the record
vendors keep in their databases, and the reason this is worth building.

Motivating evidence: SmellBench (2026) found **63.1%** of detected
"hard-severity" architectural smells were expert-judged false positives. Three of
ratchet's own five original rules were noise against a real codebase, and only
running them revealed it.

## Rule tiers and promotion

```
rules/core/    published precision data, community-maintained
rules/org/     a company's own pack
rules/local/   project-specific, in the repo being analysed
```

Resolution is local → org → core, most specific wins. A rule is **promoted on
evidence**: by default, judgements from ≥2 organisations across ≥3 repositories,
≥50 verdicts, and precision ≥0.80 (`strata promote-check`).

**Precision data is portable even when code is not.** An organisation can
contribute "47 findings, 41 confirmed, 6 false positives" for a rule without
sharing a line of source. That is what lets closed-source teams participate in an
open corpus.

## Tickets that close themselves

```sh
ratchet scan . --emit findings --repo myservice | docket plan
```
```
5 tickets covering 208 findings (grouped by theme)

 1. myservice: Fix 97 × any-in-exported-signature     97 findings
 2. myservice: Fix 90 × naked-type-assertion          90 findings
 3. myservice: Fix 16 × else-after-return             16 findings
```

**208 findings, 5 tickets.** Ninety naked type assertions is one ticket and one
agent PR, not ninety tickets.

Each ticket carries its cohort of fingerprints, so `docket sync` answers "how far
along is this" by set arithmetic against a fresh scan — and closes the ticket
when the cohort is clear:

```
T-002   Fix 90 × naked-type-assertion    47/90
T-004   Fix 3 × panic-in-library          3/3  ✓ ready to close
```

**The ratchet is what makes this closeable.** A theme ticket would normally never
finish, because new instances keep arriving. `ratchet check` blocks them, so the
cohort is fixed at creation and the ticket has a finish line.

Two refusals worth knowing: it will not ticket a finding marked `false-positive`
(that is noise reaching a human — fix the rule instead), and it will not re-file
something whose ticket someone closed. `--yes` is required to touch a tracker;
without it everything is a preview. `--provider file` writes markdown and needs
no tracker at all.

## Dependency conformance

`plumb` answers one question: **do the dependencies obey the layering we chose?**
It does not try to answer whether the layering is any good, and it does not
judge whether a package's contents belong in it — that is a different question,
about responsibility rather than reachability, and it needs a different rule. That distinction
is the whole design, and it is not fastidiousness — SmellBench (2026) found
**63.1%** of detected hard-severity architectural smells were expert-judged
false positives. Tools that look for bad architecture without being told what
good looks like produce findings nobody acts on.

So plumb needs a human declaration, and the declaration lives in the document:

````markdown
# Architecture

Dependencies point inward. The domain must be testable with no database,
no HTTP server and no clock.

```arch
layer domain   internal/domain
layer infra    internal/impl internal/store
forbid domain -> infra
```


## Why

The March incident came from a domain rule reading the database mid-evaluation,
which made evaluation order significant and non-obvious.
````

The prose and the rule cannot drift, because they are the same file and the same
review. It also means an agent reads the artefact it is bound by: a `CLAUDE.md`
describing the architecture is a *prompt*, and a block CI enforces is a *gate*.

### Checks are transitive, and that is the point

A direct-import rule — which is what most dependency linters do — has a hole:

```
internal/domain -> internal/impl              caught
internal/domain -> internal/helper -> impl    SILENT
```

Same architectural dependency, one hop away. Nobody has to be working around
anything to produce it; an ordinary "extract a helper" refactor does. plumb
walks the transitive closure and reports the **chain**, because `domain →
helper → impl` tells you where to cut and "domain violates infra" does not.

```
$ plumb scan .
18 packages, 4 rules, 1 violations

forbid domain -> infra
  internal/domain → internal/helper → internal/impl
      the indirection through internal/helper does not change the dependency;
      split internal/helper so the part internal/domain needs does not reach internal/impl
```

### Before it is worth anything

**The tool is the easy half.** Without these steps it is a placebo, and shipping
it without saying so invites exactly that.

1. **Write `ARCHITECTURE.md`, including the `## Why`.** The prose is what stops
   someone deleting a rule in eighteen months because it was in the way. A block
   with no reasons is a config file with extra steps.
2. **Declare what is already true.** A declaration that fails on the day it is
   written teaches everyone to ignore it. Start from the shape the codebase has,
   then tighten.
3. **`plumb baseline` on adoption.** Do not start from zero violations on an
   existing codebase — that is how a rule gets switched off in the first
   afternoon. Freeze what exists, fail only what is new.
4. **CODEOWNERS on the declaration, with a *different and smaller* group than
   the code owners.** If the same person approves both the code and the rule it
   broke, the separation is cosmetic.
5. **CODEOWNERS must own itself**, or the guard is editable by the thing it
   guards.
6. **Branch protection with no admin bypass.** Otherwise every gate here is
   advisory.
7. **Run it before opening the PR, not only in CI.** A constraint hit during
   design is a redirect; hit at review it is an obstacle to route around.

```
# CODEOWNERS
/ARCHITECTURE.md        @org/architecture
/.plumb-baseline.json   @org/architecture
/CODEOWNERS             @org/architecture
/.github/workflows/     @org/architecture
```

### Changing the architecture: make tightening free

Architecture evolves, and a change process people route around is worse than
none. So the cost is asymmetric, and `plumb diff` decides which side you are on:

```sh
plumb diff . --base origin/main
```

**Tightening** — adding a `forbid`, widening a layer so more code is covered —
exits 0. It forbids strictly more; nobody needs protecting from it.

**Loosening** — removing a `forbid`, narrowing a layer so packages quietly leave
its rules — exits non-zero, so CI can require the second reviewer only where it
matters. Note that narrowing is a loosening even though the file gets shorter:
length is not the signal.

Reordering statements is **no change**. If cosmetic edits demanded a reviewer,
the process would be ignored for the edits that count.

### Where the real risk is

Measured on a 613-commit AI-assisted Go service that had layering rules:

| route around the gate | observed |
|---|---|
| the rule does not express what you meant | **found in five minutes** — the transitive hole above |
| the check is not run at all | silent by construction |
| suppression | 122 `//nolint`, **none** on the layering rules |
| editing the declaration | **0** — touched twice, both tightening, 10 insertions and 0 deletions |

**Tampering is the smallest risk, not the largest.** It is the loudest possible
act: a diff to a named file, in the PR, trivially owned by CODEOWNERS. The
failures that actually happen are silent — a rule that does not say what you
meant, or a check nobody wired up. Spend the effort there.

The same data suggests why structural constraints hold better than local ones.
Suppressions on that service grew 0 → 122 in three months, all on local lints
(`errcheck` 44, `gosec` 23, `wrapcheck` 13). A layering rule fires as you write
the import, when redirecting is nearly free; a local lint fires after the logic
exists, when suppressing is cheaper than fixing.

And the ratchet removes most of the motive before you reach for a lock: you
never need to weaken a rule to land a PR, only to avoid adding new violations.


### Ownership: what a layer may declare

Layering answers "may cart reach rating". It cannot answer "does this belong in
cart". Rating logic inside a cart service can be perfectly layered and still be
in the wrong place, and no import graph will see it. What sees it is vocabulary:
what the code *declares*, not what it imports.

```
owns cart     cart line-item checkout
owns rating   rating review score
```

A type or function declared in `cart` whose name carries rating's vocabulary is
a `responsibility-drift` finding. Cart *calling* rating's API is fine, and
layering already governs it. A layer with no `owns` line is a consumer and is
never checked, which is what keeps a presentation layer that legitimately
declares a `RatingPage` quiet.

The vocabulary is written by a human, because it is intent, and intent is the
one thing a scan cannot recover from code. `plumb learn` drafts a starting list
from what each layer declares — roughly half right in practice, which is the
point: a list to strike through rather than a blank page. The misses are
instructive: homonyms ("sheet" as a spreadsheet and as a bottom sheet), and
stems that are noise in one codebase and a concept in another. One line of
human judgement resolves each.

`owns` works on Go, C#, Dart, TypeScript, Java, Kotlin and Python, and does not
need a Go module. Adding a term is a tightening; removing or transferring one is
a loosening that `plumb diff` sends to a second reviewer. Adopt it the same way
as layering: draft, edit, `plumb baseline`, and the ratchet blocks new drift
from then on.

#### Choosing what to own

Measured against six real codebases in three languages, almost all the noise
came from four causes. All four are avoidable, and none of them is obvious.

**Owning a term and being checked are the SAME SWITCH.** A layer is checked for
drift if and only if it has an `owns` line. There is no way to say "impl owns
`outbox`, but do not scrutinise impl". That coupling produces the single
biggest false-positive class:

> A persistence layer for memberships necessarily declares `MembershipRepo`.
> An adapter for vouchers necessarily declares `ApplyVoucher`. A mapper for
> eligibility necessarily declares `EligibilityMapper`. That is correct
> hexagonal architecture, not drift.

On one service, giving the infrastructure layer an `owns` line produced **39
findings of which 38 were this class**. On another, 21 of 21.

**So own the terms whose misplacement you want to CATCH, not the terms that
describe the layer.** Narrowing one declaration from `owns domain membership
benefit entitlement` to `owns domain entitlement` — a word the infrastructure
layer never says — took it from 39 findings to 1, and the 1 was real.

This is the opposite of what `plumb learn` drafts, because learn ranks by
frequency and the most frequent terms are exactly the ones adapters also use.
Treat the draft as a list of candidates to narrow, not to accept.

**Give adapters, mappers and repositories no `owns` line at all.** They are
consumers. Reserve ownership for the layers that are supposed to be PURE — the
ones where a foreign concept appearing is genuinely news.

**Match `--depth` to how the repository is laid out.** The default of 2 finds
contexts in a service and useless ones in a monorepo: on a Flutter repo it
nominated `packages/features` as a single context holding 18,606 declarations,
while `--depth 3` gave `qcommerce`, `subscription` and `dine_out` with real
vocabulary. Run `learn` at two depths and keep the one whose layer names you
recognise.

**Generated files are excluded automatically** — `*_gen.go`, `*.pb.go`,
`*.g.dart`, `*.freezed.dart`, `*.designer.cs` and friends. Their names come from
a schema rather than from anyone's intent, and no finding in them is actionable
because nobody can move the declaration. This is not marginal: on one service
10 of 17 findings came from generated analytics events, and a Flutter monorepo
carries 310 such files.

#### What it costs to read

Expect to reject most of the first run and keep the declaration that survives.
The terms that produce false positives are recognisable: generic verbs
(`modify`, `apply`, `enrich`, `handle`), words a homonym makes ambiguous across
domains, and anything an adapter would naturally say. The terms that produce
real findings are the ones only one part of the system has any business using.


### Judgement: does it make sense here

Both plumb rules are blind to a function named `ApplyDiscount` in cart that
actually averages ratings. `judge` reads the body and the prose of
ARCHITECTURE.md and asks one question per declaration: does this belong in the
layer it sits in, and which sentence of the document says so.

```sh
judge scan . --base origin/main --provider anthropic          # this PR
judge scan . --provider gemini --model gemini-2.5-pro --emit findings | ratchet import -
```

It is built as one more linter, not an oracle. Every finding must quote a
sentence that exists verbatim in the document, or it is dropped and counted.
The fingerprint is the symbol, not the explanation, so the baseline survives a
model that words things differently each run. The rule id carries the prompt
version, so precision is measured per prompt and per model like any other
rule — which is how you find out whether a local model is good enough for your
repository, rather than arguing about it. Findings are warnings until a team
has that number and chooses to gate.

Responses are cached on disk, so a rerun on an unchanged tree costs nothing
and a whole-repo pass is incremental.

**Two ways to pay, one toggle.** `--provider` (or `JUDGE_PROVIDER`) selects
who answers:

| provider | bills against | when |
|---|---|---|
| `anthropic`, `gemini`, `openai` (Codex models by name) | an API key, per token — `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `OPENAI_API_KEY`; anthropic also honours an `ant auth login` profile | a team with API billing; CI |
| `claude-code` (aliases `plan`, `max`) | a Claude subscription, via the Claude Code CLI in headless mode | a person with a plan and no credits |

```sh
JUDGE_PROVIDER=anthropic   judge scan . --base origin/main     # at work: per-token
JUDGE_PROVIDER=claude-code judge scan . --base origin/main     # at home: the plan
```

The `claude-code` provider batches several declarations per call, because
each call carries Claude Code's own context and a plan is a usage window
rather than a per-token price. It gives the model no tools: the question is
answered from the excerpt, exactly as with the API providers, so the two are
comparable.

The third path is the **judge skill** in `.claude/skills/judge`, for when you
want Claude Code to read *further* than the excerpt — follow a call, open the
test — on the plan. It lists the cases with `judge cases`, judges them in the
session, and hands its answers to `judge verify`, which applies the same
citation and layer checks and emits the same findings. Copy the directory to
`~/.claude/skills/judge/` once and it is available in every repository.

## Storage: files

```
.assay/
  measures/2026/09/19.jsonl      date-partitioned, append-only
  findings/2026/09/19.jsonl
  verdicts/verdicts.jsonl        append-only log, last write wins
  tickets/tickets.jsonl          append-only log of ticket events
  rollup/                        derived cache, always rebuildable
```

No database. Raw data is the truth and rollups are a cache, so there is no state
that cannot be recomputed. Partitioning by date makes a range query a directory
listing — no index. And every file is readable with `grep` and `jq` without any
binary here, which is the durability property that matters when tools get
abandoned or relicensed.

Scale: ~12,500 records across three repos is 3 MB. Five repos over five years
with function-level detail lands in the low hundreds of MB.

## How fast

Measured on assay itself — 73 Go files, 18,600 lines, 20 packages — on a
laptop (i9-13900H), best of three, wall clock:

| | |
|---|---|
| `ratchet scan .` (findings + project metrics) | 40 ms |
| `ratchet scan . --detail --emit measures` (1,383 function-level records) | 50 ms |
| `ratchet check .` | 40 ms |
| `ratchet history` | ~14 ms per commit, in parallel |
| `strata append` / `query` / `stat` over that store | 30–40 ms each |
| `lens top`, `docket plan` | 20–30 ms |
| `plumb scan .` / `check .` (import graph, transitive) | 250–330 ms — almost all of it is `go list` |
| `plumb learn` — ownership draft over a 150-file, 29,000-line Dart tree | 40 ms |
| `judge cases .` | 30 ms |
| `judge scan .`, 189 declarations, live through Claude Code | **156 s**, 24 batched calls |
| `judge scan .` again, unchanged tree | 30 ms — every answer from the cache |

Everything deterministic is bounded by reading the files once. The one slow
tool is the judge, and it is slow because a model is thinking: roughly six
seconds per batch of eight declarations, in three parallel calls. On a pull
request with `--base` it judges only the declarations in changed files, which
is usually a handful. On a whole repository, run it once, and the cache makes
every rerun free until the code or the document changes.

## Install

```sh
go install github.com/sherzing/assay/cmd/ratchet@latest
go install github.com/sherzing/assay/cmd/strata@latest
go install github.com/sherzing/assay/cmd/lens@latest
go install github.com/sherzing/assay/cmd/docket@latest
go install github.com/sherzing/assay/cmd/plumb@latest
go install github.com/sherzing/assay/cmd/judge@latest
```

Single Go module, one binary per `cmd/` — install only what you want.

**Five of the six link nothing outside the standard library.** `ratchet`,
`strata`, `lens`, `docket` and `plumb` have no third-party code in them at all,
which is the property that matters for a tool you are asked to run in CI
against your own source.

`judge` is the exception: it uses the Anthropic Go SDK to call a model, which
brings in eleven transitive modules (see [NOTICE](NOTICE)). It is also the only
tool that talks to anything outside your machine, so the dependency and the
network call arrive together rather than by surprise. Nothing is vendored.

The shared packages are an implementation convenience; the interop contract is
the JSONL.

Building locally instead:

```sh
make build        # every tool into ./bin, which is gitignored
make check        # what CI runs: build, -race tests, vet, gofmt, self-check
make cover        # coverage, including the out-of-process cmd/ tests
```

## It measures itself

assay runs on assay. The first self-scan put `Scan` at cognitive **41** — the
worst function in its own codebase — and `emitAssay`, written the same day, at
**28**. Both were genuinely doing too much.

Splitting them moved the codebase:

| | before | after |
|---|---|---|
| cognitive max | 41 | **27** |
| cognitive mean | 5.76 | **5.08** |
| cyclomatic max | 25 | **17** |
| nesting max | 4 | **3** |
| findings | 5 | **3** |

The two fewer findings were not the goal — they fell out of the restructure,
because the error paths that had been swallowed inside a walk closure became
honest return values once the function was split.

If it cannot hold its own line, it has no standing to hold anyone else's.

## Licence

[Apache License 2.0](LICENSE).

Chosen over MIT for two clauses that matter to a corpus built from many
organisations: the **patent grant** (§3), so a company contributing a rule knows
what it is granting and receiving, and **§5**, which puts contributions under the
same licence automatically — so there is no CLA standing between a team and a
pull request.
