# Knowledge graph: route before reading

A persistent map of the repository, so Claude reads the few lines a task needs
instead of grepping and opening whole files. Built with
[graphify](https://pypi.org/project/graphifyy/) 0.9.66 (AST extraction, no
LLM); kept current by `tools/kg/kg_sync.py` and two Claude Code hooks.

## What is in it

- **Nodes:** about 5,050. Every function, method, type and file in Go,
  JavaScript and Python, plus every heading in the Markdown docs.
- **Edges:** about 16,200. Calls, imports, references, and containment.
  Each edge carries a source file and line, and a confidence of `EXTRACTED`
  or `INFERRED`.
- **Communities:** about 190, detected by Leiden and named.
- **Excluded:** `.graphifyignore` keeps vendored bundles and test output out.
  Without it, the vendored bundles add 700 nodes of noise.
- **Not stored:** secrets, and anything `.gitignore` covers.

Everything lives in `graphify-out/`, which is gitignored and local to each
machine:

| Record | File |
|---|---|
| The current graph | `graph.json` |
| The community report | `GRAPH_REPORT.md` |
| The last sync's checkpoint | `CHECKPOINT.md` |
| Every sync's record | `history.jsonl` |

`CHECKPOINT.md` and `history.jsonl` are kept separately from the current
graph. Each sync records what it changed:

- the files added, modified and deleted;
- the nodes added and removed;
- the node and edge counts before and after;
- git branch and HEAD, and the uncommitted paths.

## Routing

    & (gc graphify-out\.graphify_python) tools\kg\kg_sync.py route "<words or a symbol>"

It ranks symbols by how well their name, file and community match the words.
A symbol named exactly ranks first. It prints:

- the files to read first, with the matching symbols and line numbers;
- the files connected to those by calls or imports.

### Measured on three real tasks

Tokens are counted as characters ÷ 4.

| Task | `route` | `graphify query` | grep hits | top 3 files read whole |
|---|---|---|---|---|
| WebRTC stall: receiver loss → bandwidth estimate | 403 | 1,647 | 827 | 10,722 |
| A poll tally going backwards under concurrent votes | 314 | 1,633 | 1,299 | 11,961 |
| The meeting chat panel's unread counter | 442 | 1,625 | 9,658 | 91,311 |

- **Speed:** `route` answers in about 50 ms in process, or 200 ms with
  interpreter start-up.
- **Accuracy:** in every task the first three files listed are the ones the
  work happened in, with line numbers: `bwe.go` and `peer.go` for the
  estimator; `hub.go`, `pushorder_test.go` and `polls.go` for the tally;
  `app.js`, `meetingchat.go` and `chat.js` for the unread counter.
- **graphify's own benchmark:** `graphify benchmark` reports 13.8× fewer
  tokens per query than reading the corpus. That compares against the whole
  corpus (about 337k tokens), so it overstates the saving against a
  competent grep.

## Staying in sync

**How a change is found.** The snapshot holds the mtime every graphed file
had at the last good sync, and the disk is diffed against it. This catches
edits from anywhere: Claude, an editor, or git (checkout, pull, stash,
commit), uncommitted work included. A full scan takes 0.5 s, so no per-edit
hook or queue is needed.

**What a sync re-extracts.** Only the changed files. graphify carries every
other file's nodes forward, and one sync takes 6–10 s.

**The hooks** are in `.claude/settings.local.json`, run in PowerShell, and do
nothing on a machine without a graph:

| Hook | When it runs | What it does | Cost |
|---|---|---|---|
| SessionStart | Every session start | Sync, then inject a short checkpoint as context | Blocking; 1.2 s when idle |
| Stop | After every turn | Sync in the background | Non-blocking |

**Safety.** Every sync takes a lock, backs up the graph and validates the
result. Validation rejects:

- duplicate node ids, and edges that point at missing nodes;
- nodes that still claim a deleted file;
- a changed file that produced no nodes.

When a sync is rejected, the backup is restored, the checkpoint says why, and
the change stays pending for the next sync.

**Community names.** Leiden re-clusters the whole graph on every rebuild.
Adding one file moved the count from 185 to 194 communities, and graphify
renamed 103 of them after their hub nodes. A registry of curated names
(`.kg_names.json`) fixes that:

- each new community gets the curated name of the registered community it
  is at least half of (Jaccard ≥ 0.5, one to one);
- a matched entry rolls forward to the community's current members;
- an unmatched entry stays registered, so a name comes back when its
  community does.

`graphify cluster-only` then writes the names into the outputs. Leiden is
seeded, so it reproduced the identical partition. 146 names survived instead
of 82.

## Acceptance test

Run in a scratch git worktree, starting from a clean full build of 5,026
nodes. The real source was not touched.

| Step | Result |
|---|---|
| Rename `validatePoll` → `checkPollShape` in `polls.go` | modified: old node gone, new node present |
| Create `kgprobe.go` with `KGProbe()` calling `pollOf` | added: nodes present; `route KGProbe` finds it first |
| Delete `sfu/keyframereq_test.go` | deleted: all 11 of its nodes gone |
| Run the SessionStart path | valid hook JSON; 1 added, 1 modified, 1 deleted, **382 files not re-indexed, 0 nodes changed outside the change set**, 10.3 s |
| Force validation to fail | `graph.json` byte-identical afterwards, change still pending; the next sync applied it |
| Fresh checkout, every mtime new | every file re-indexed: correct, since the snapshot cannot vouch for any file |

## Limits, measured

- **Go calls.** Calls through a receiver (`s.pushMessage(...)`) are often
  unresolved:
  - Of the Go functions called somewhere in the source, the graph has a
    caller edge for 57%.
  - `.pushMessage()` has 7 call sites and no caller edge.
  - So `graphify affected` under-reports for Go methods: grep the callers
    before changing a shared method.
- **Callers by language.** The share of functions and methods with at least
  one known caller: Go 46% of methods and 56% of functions; JS 57% and 75%;
  `.mjs` test harnesses 26% and 27%.
- **Docs.** Markdown has heading nodes only. Semantic extraction needs an
  LLM pass and was not run.
- **Community names.** Communities that move by more than half get hub
  names, such as `Router` or `Service`, until they are curated again.

## Commands

| Command | What it does |
|---|---|
| `/kg-route` | Route a question to files and lines |
| `/kg-status` | Show what is stale; changes nothing |
| `/kg-update` | Sync now |
| `/kg-rebuild` | Re-extract every file |
| `/kg-query` | `graphify query` |
| `/kg-explain` | `graphify explain` |
| `/kg-impact` | `graphify affected`, with the Go caveat |

To enable the hooks on another machine:

1. Install graphify: `uv tool install graphifyy==0.9.66`.
2. Build once.
3. Copy the `hooks` block from `.claude/settings.local.json`.
