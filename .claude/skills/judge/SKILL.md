---
name: judge
description: Judge whether declarations belong in the group they sit in, according to ARCHITECTURE.md, and emit assay findings. Use when asked to review architecture intent, responsibility drift, or "does this code belong here" over a repo or a branch.
---

# judge — responsibility review, by Claude Code

You are the model behind assay's `judge` tool, running on the user's Claude Code
plan instead of an API key. The difference from `judge scan` is that you may
read further than the excerpt: follow a call, open the test, check the git log.
Everything else is the same: the same question, the same rules, the same
verification, and the output lands in the same finding records.

## Steps

1. List the cases. From the repository root (or the directory the user names):

       judge cases . > /tmp/judge-cases.jsonl          # whole tree
       judge cases . --base origin/main > /tmp/judge-cases.jsonl   # this branch only

   Each line is `{"decl":{"Group","Name","File","Line"},"excerpt":"..."}`.
   If `judge` is not on PATH, build it: `go build ./cmd/judge` in the assay repo.

2. Read `ARCHITECTURE.md`. The prose above the ` ```arch ` block is the intent;
   the block names the groups and what each owns. The document is the only
   authority. Your own opinion of good design is not.

3. For each case, decide, following these rules exactly:
   - The declaration belongs unless the document gives a specific reason it
     does not. Judge responsibility, not dependencies: what the code imports
     is not your concern; whether this concept is this group's job is.
   - When it does not belong, name the group it belongs in, using only the
     group names in the block.
   - Quote one sentence from the document, verbatim, that supports the
     judgement. Copy it exactly. A paraphrase is discarded by the verifier.
     If no sentence supports a finding, the declaration belongs.
   - One sentence of reasoning, naming the concept that is out of place.
   Read the surrounding code when the excerpt is not enough. Do not edit
   anything.

4. Write one JSON object per case to `/tmp/judge-answers.jsonl`:

       {"file":"internal/cart/x.go","line":9,"name":"AverageStars","in":"cart",
        "belongs":false,"group":"rating","reason":"averages ratings","citation":"<verbatim sentence>"}

   `file`, `line`, `name` and `in` are copied from the case. Include every
   case, with `"belongs":true` and empty `group`/`reason`/`citation` when it
   belongs.

5. Verify and emit:

       judge verify . --answers /tmp/judge-answers.jsonl --model claude-code-skill
       judge verify . --answers /tmp/judge-answers.jsonl --model claude-code-skill --emit findings > judged.jsonl

   The verifier drops answers whose citation is not in the document or whose
   group is not declared, and says so. Report the findings and the dropped
   lines to the user; a high drop rate means the document does not say
   enough, which is a fact about the document.

## Install for every repository

Copy this directory to `~/.claude/skills/judge/`. Personal skills are available
in every repository Claude Code opens, so the skill does not need to live in
the repositories it reviews.
