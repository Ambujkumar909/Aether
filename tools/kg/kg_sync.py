"""Keep graphify-out/graph.json in step with the working tree.

    sync     re-index the files changed since the last sync   (Stop hook, /kg-update)
    start    sync, then print a short checkpoint as context   (SessionStart hook)
    status   say what is stale, change nothing                (/kg-status)
    full     re-extract every file                            (/kg-rebuild)

A change is any graphed file whose mtime differs from the snapshot taken at the
last good sync, so edits count wherever they came from: Claude, an editor, git
checkout / pull / stash / commit, uncommitted work included. Only those files
are re-extracted; graphify carries every other file's nodes forward.

A sync backs the graph up first, validates the result, and restores the backup
if validation fails, so graph.json is always the last graph that passed.
Run it with graphify's own interpreter (graphify-out/.graphify_python).
"""
import contextlib
import json
import os
import shutil
import subprocess
import sys
import time
from collections import Counter
from pathlib import Path

# Community detection iterates sets, so the hash seed must be fixed or ids
# and labels drift between runs. os.exec* detaches on Windows; re-run instead.
if os.environ.get("PYTHONHASHSEED") != "0":
    env = {**os.environ, "PYTHONHASHSEED": "0"}
    sys.exit(subprocess.run([sys.executable, *sys.argv], env=env).returncode)

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / "graphify-out"
GRAPH = OUT / "graph.json"
SNAP = OUT / ".kg_snapshot.json"
BACKUP = OUT / ".kg_backup"
LOCK = OUT / ".kg_sync.lock"
CHECKPOINT = OUT / "CHECKPOINT.md"
HISTORY = OUT / "history.jsonl"
NAMES = OUT / ".kg_names.json"  # curated community names -> members
KEPT = ["graph.json", "manifest.json", "GRAPH_REPORT.md",
        ".graphify_labels.json", ".graphify_labels.json.sig"]
CAP = 15  # names listed per section of the checkpoint


def tracked():
    """{repo-relative path: mtime} for every file graphify extracts without an LLM."""
    from graphify.detect import detect
    from graphify.extract import _get_extractor
    d = detect(ROOT)["files"]
    files = list(d.get("code", [])) + [p for p in d.get("document", []) if _get_extractor(Path(p))]
    now = {}
    for p in map(Path, files):
        p = p if p.is_absolute() else ROOT / p
        try:
            now[p.resolve().relative_to(ROOT).as_posix()] = p.stat().st_mtime
        except (OSError, ValueError):
            pass
    return now


def snapshot():
    if SNAP.exists():
        return json.loads(SNAP.read_text("utf-8"))
    # First run: graphify's manifest holds each file's mtime at the last build.
    man = OUT / "manifest.json"
    m = json.loads(man.read_text("utf-8")) if man.exists() else {}
    return {k: v.get("mtime") for k, v in m.items()}


def changes(snap, now):
    added = sorted(set(now) - set(snap))
    modified = sorted(k for k in set(now) & set(snap)
                      if snap[k] is None or abs(now[k] - snap[k]) > 1e-3)
    # The manifest also lists files we do not track (images); gone means gone from disk.
    deleted = sorted(k for k in set(snap) - set(now) if not (ROOT / k).exists())
    return added, modified, deleted


def load_graph():
    return json.loads(GRAPH.read_text("utf-8"))


def src(n):
    return (n.get("source_file") or "").replace("\\", "/")


def node_index(g):
    # Community ids are excluded: re-clustering renumbers them across the whole
    # graph, which is not a change to any file's contents.
    return {n["id"]: (n.get("label"), src(n), n.get("source_location")) for n in g["nodes"]}


def validate(g, present, deleted):
    problems = []
    ids = [n["id"] for n in g["nodes"]]
    if not ids:
        problems.append("graph is empty")
    if len(ids) != len(set(ids)):
        problems.append(f"{len(ids) - len(set(ids))} duplicate node ids")
    known = set(ids)
    dangling = sum(1 for e in g.get("links", []) if e["source"] not in known or e["target"] not in known)
    if dangling:
        problems.append(f"{dangling} edges point at missing nodes")
    per_file = Counter(src(n) for n in g["nodes"])
    stale = [f for f in deleted if per_file.get(f)]
    if stale:
        problems.append(f"nodes still claim deleted files: {stale[:5]}")
    empty = [f for f in present if not per_file.get(f)]
    if empty:
        problems.append(f"changed files produced no nodes: {empty[:5]}")
    return problems


def git(*args):
    try:
        return subprocess.run(["git", *args], cwd=ROOT, capture_output=True, text=True,
                              timeout=10).stdout.strip()
    except (OSError, subprocess.TimeoutExpired):
        return ""


class Lock:
    """One sync at a time: the Stop hook runs in the background and can overlap
    the next session's start. An OS lock, so a run killed by a hook timeout
    releases it by dying instead of leaving a lock file behind."""

    def __enter__(self):
        self.fh = open(LOCK, "a+")
        deadline = time.time() + 300
        while True:
            try:
                if os.name == "nt":
                    import msvcrt
                    self.fh.seek(0)
                    msvcrt.locking(self.fh.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl
                    fcntl.flock(self.fh, fcntl.LOCK_EX | fcntl.LOCK_NB)
                return self
            except OSError:
                if time.time() > deadline:
                    self.fh.close()
                    raise TimeoutError("another sync has held the lock for five minutes")
                time.sleep(0.5)

    def __exit__(self, *exc):
        self.fh.close()  # closing the handle releases the lock


def backup():
    BACKUP.mkdir(exist_ok=True)
    for name in KEPT:
        if (OUT / name).exists():
            shutil.copy2(OUT / name, BACKUP / name)


def restore():
    for name in KEPT:
        if (BACKUP / name).exists():
            shutil.copy2(BACKUP / name, OUT / name)


def rebuild(paths):
    """paths=None re-extracts everything; otherwise only these files."""
    from graphify.watch import _rebuild_code
    os.chdir(ROOT)  # the graph was built from "." and stores paths relative to it
    return _rebuild_code(Path("."), changed_paths=None if paths is None else [Path(p) for p in paths],
                         force=paths is None, block_on_lock=True)


def communities(g):
    groups, name = {}, {}
    for n in g["nodes"]:
        c = n.get("community")
        if c is not None:
            groups.setdefault(c, set()).add(n["id"])
            name[c] = n.get("community_name")
    return groups, name


def registry(before):
    """Curated community names and the members each had when last matched.

    Started from the graph as it stood, keeping only names that are not simply
    a member's own label (graphify's fallback is the hub node's label)."""
    if NAMES.exists():
        return {k: set(v) for k, v in json.loads(NAMES.read_text("utf-8")).items()}
    def bare(s):  # hub names drop the "()" and leading "." that member labels carry
        return (s or "").strip(".").removesuffix("()")
    groups, name = communities(before)
    label = {n["id"]: bare(n.get("label")) for n in before["nodes"]}
    return {name[c]: m for c, m in groups.items()
            if name[c] and bare(name[c]) not in {label[i] for i in m}}


def carry_labels(reg, after):
    """Curated names for `after`'s communities: (names to change, members, names now).

    Any edit that adds or removes a symbol changes some community's members, and
    graphify then swaps that community's curated name for its hub node's name.
    A name describes the community, not its exact member list, so a community
    that is at least half a registered one (Jaccard >= 0.5, best match first,
    one to one) gets its name. Matched entries roll forward to the current
    members, so gradual drift is followed; unmatched entries stay, so a name
    whose community comes back gets it back instead of being lost for good."""
    new, new_name = communities(after)
    reg_of = {}
    for name, members in reg.items():
        for i in members:
            reg_of.setdefault(i, []).append(name)
    pairs = []
    for c, members in new.items():
        for name, k in Counter(nm for i in members for nm in reg_of.get(i, [])).items():
            pairs.append((k / (len(members) + len(reg[name]) - k), c, name))
    taken_new, taken_name, carried = set(), set(), {}
    for j, c, name in sorted(pairs, key=lambda p: (-p[0], p[1], p[2])):
        if j < 0.5:
            break
        if c in taken_new or name in taken_name:
            continue
        taken_new.add(c)
        taken_name.add(name)
        reg[name] = new[c]
        if name != new_name[c]:
            carried[c] = name
    return carried, new, new_name


def apply_labels(after, before):
    """Write carried names into graphify's labels file, with signatures for the
    current members, then let `graphify cluster-only` rewrite graph.json, the
    report and the HTML with them. Leiden is seeded, so re-clustering the same
    graph reproduces the same communities and the signatures match (measured:
    identical partition, 146 of 194 names curated instead of 82)."""
    from graphify.cluster import community_member_sigs
    reg = registry(before)
    carried, groups, names_now = carry_labels(reg, after)
    NAMES.write_text(json.dumps({k: sorted(v) for k, v in sorted(reg.items())}), encoding="utf-8")
    if not carried:
        return 0
    labels = {**names_now, **carried}
    (OUT / ".graphify_labels.json").write_text(json.dumps(
        {str(k): v for k, v in sorted(labels.items())}, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (OUT / ".graphify_labels.json.sig").write_text(json.dumps(
        {str(k): v for k, v in community_member_sigs(groups).items()}), encoding="utf-8")
    subprocess.run([sys.executable, "-m", "graphify", "cluster-only", ".", "--no-label"],
                   cwd=ROOT, stdout=sys.stderr, stderr=sys.stderr, check=True)
    return len(carried)


def record(before, after, added, modified, deleted, now, problems, secs):
    b, a = node_index(before), node_index(after)
    gone, new = set(b) - set(a), set(a) - set(b)
    moved = {i for i in set(a) & set(b) if a[i] != b[i]}
    touched = Counter()
    for i in gone | moved:
        touched[b[i][1]] += 1
    for i in new | moved:
        touched[a[i][1]] += 1
    changed = set(added) | set(modified) | set(deleted)
    # Import specifiers such as node:fs are nodes too, but not files anyone edited.
    outside = sorted(f for f in touched if f not in changed and (ROOT / f).is_file())
    dirty = [line[3:] for line in git("status", "--porcelain").splitlines()]
    entry = {
        "at": time.strftime("%Y-%m-%d %H:%M:%S"),
        "branch": git("rev-parse", "--abbrev-ref", "HEAD"),
        "head": git("rev-parse", "--short", "HEAD"),
        "uncommitted": dirty,
        "added": added, "modified": modified, "deleted": deleted,
        "untouched_files": len(now) - len(set(added) | set(modified)),
        "nodes": [len(b), len(a)], "edges": [len(before.get("links", [])), len(after.get("links", []))],
        "nodes_added": sorted(f"{a[i][0]} ({a[i][1]})" for i in new),
        "nodes_removed": sorted(f"{b[i][0]} ({b[i][1]})" for i in gone),
        "communities": [len(communities(before)[0]), len(communities(after)[0])],
        "names_not_carried": sorted(set(communities(after)[1].values()) - set(communities(before)[1].values())),
        "files_with_node_changes": dict(touched.most_common()),
        "node_changes_outside_change_set": outside,
        "problems": problems,
        "seconds": round(secs, 2),
    }
    with HISTORY.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(entry, ensure_ascii=False) + "\n")
    CHECKPOINT.write_text(render(entry), encoding="utf-8")
    return entry


def names(items):
    more = f" … +{len(items) - CAP} more" if len(items) > CAP else ""
    return ", ".join(items[:CAP]) + more if items else "none"


def render(e):
    state = "FAILED, previous graph restored: " + "; ".join(e["problems"]) if e["problems"] else "passed validation"
    return "\n".join([
        "# Knowledge graph checkpoint",
        "",
        f"- **When:** {e['at']} on `{e['branch']}` @ `{e['head']}`, {len(e['uncommitted'])} uncommitted paths",
        f"- **Result:** {state} in {e['seconds']} s",
        f"- **Re-indexed:** {len(e['added'])} added, {len(e['modified'])} modified, {len(e['deleted'])} deleted;"
        f" {e['untouched_files']} files not re-indexed",
        f"- **Graph:** {e['nodes'][0]} → {e['nodes'][1]} nodes, {e['edges'][0]} → {e['edges'][1]} edges",
        f"- **Communities:** {e['communities'][0]} → {e['communities'][1]};"
        f" names not in the previous graph: {names(e['names_not_carried'])}",
        f"- **Node changes outside the change set:** {names(e['node_changes_outside_change_set'])}",
        "",
        f"Added files: {names(e['added'])}",
        f"Modified files: {names(e['modified'])}",
        f"Deleted files: {names(e['deleted'])}",
        f"Nodes added: {names(e['nodes_added'])}",
        f"Nodes removed: {names(e['nodes_removed'])}",
        "",
    ])


def sync(full=False):
    if not GRAPH.exists():
        return "no graph yet: run /kg-rebuild"
    with Lock():
        now = tracked()
        added, modified, deleted = changes(snapshot(), now)
        if not full and not (added or modified or deleted):
            return "current"
        before = load_graph()
        backup()
        t = time.time()
        paths = None if full else added + modified + deleted
        ok = rebuild(paths)
        try:
            after = load_graph()
            if ok and apply_labels(after, before):
                after = load_graph()
            problems = [] if ok else ["graphify rebuild reported failure"]
            problems += validate(after, [] if full else added + modified, deleted)
        except (OSError, ValueError) as exc:
            after, problems = before, [f"graph.json unreadable after rebuild: {exc}"]
        if problems:
            restore()  # the snapshot is left alone, so the next sync retries these files
        else:
            SNAP.write_text(json.dumps(now), encoding="utf-8")
        # On failure this records the rejected attempt, so the checkpoint says what went wrong.
        e = record(before, after, added, modified, deleted, now, problems, time.time() - t)
        return ("FAILED: " + "; ".join(problems)) if problems else (
            f"re-indexed {len(added) + len(modified) + len(deleted)} files in {e['seconds']} s")


def status():
    if not GRAPH.exists():
        return "no graph yet: run /kg-rebuild"
    added, modified, deleted = changes(snapshot(), tracked())
    g = load_graph()
    head = f"{len(g['nodes'])} nodes, {len(g.get('links', []))} edges, built at commit {g.get('built_at_commit', '?')[:7]}"
    if not (added or modified or deleted):
        return f"current: {head}"
    return (f"stale: {head}\n  added: {names(added)}\n  modified: {names(modified)}\n"
            f"  deleted: {names(deleted)}")


STOP = set("the a an and or of to in on for with how does do is are what where which when why who this that from by it its into".split())


def route(question, top=8):
    """Files and lines to read for a question: symbols whose name, file or community
    match its words, grouped by file and ranked, plus the files they call or are
    called by. A reading list, not an answer: open the lines it names."""
    import re
    words = [w for w in re.findall(r"[a-z0-9]+", re.sub(r"([a-z])([A-Z])", r"\1 \2", question).lower())
             if len(w) > 2 and w not in STOP]
    if not words:
        return "route: give a few words or a symbol name"
    exact = {t.lower() for t in re.findall(r"[A-Za-z_][A-Za-z0-9_]*", question)}  # a symbol named outright
    g = load_graph()
    hits = {}
    for n in g["nodes"]:
        f = src(n)
        if not f:
            continue
        label = n.get("label") or ""
        name = re.sub(r"([a-z])([A-Z])", r"\1 \2", label).lower()
        place, comm = f.lower(), (n.get("community_name") or "").lower()
        s = sum(3 * (w in name) + (w in place) + (w in comm) for w in words)
        s += 50 * (label.strip(".").removesuffix("()").lower() in exact)
        if s:
            hits[n["id"]] = (s, n)
    best = sorted(hits.values(), key=lambda x: -x[0])[:60]
    files = {}
    for s, n in best:
        files.setdefault(src(n), [0, []])
        files[src(n)][0] += s
        files[src(n)][1].append(f"{n['label']} {n.get('source_location') or ''}".strip())
    ranked = sorted(files.items(), key=lambda kv: -kv[1][0])[:top]
    chosen = {f for f, _ in ranked}
    ids = {n["id"] for _, n in best}
    by_id = {n["id"]: n for n in g["nodes"]}
    near = Counter()
    for e in g.get("links", []):
        if e.get("relation") not in ("calls", "indirect_call", "imports", "imports_from", "references"):
            continue
        for a, b in ((e["source"], e["target"]), (e["target"], e["source"])):
            if a in ids and b in by_id:
                f = src(by_id[b])
                if f and f not in chosen and (ROOT / f).is_file():
                    near[f] += 1
    out = [f"Read first ({', '.join(words)}):"]
    for f, (s, syms) in ranked:
        comm = next((n.get("community_name") for _, n in best if src(n) == f), "")
        out.append(f"  {f}  [{comm}]  " + "; ".join(dict.fromkeys(syms[:6])))
    if near:
        out.append("Connected by calls/imports: " + ", ".join(f for f, _ in near.most_common(6)))
    return "\n".join(out)


def quiet_sync(full=False):
    # graphify reports progress on stdout; a SessionStart hook's stdout must be its JSON alone.
    with contextlib.redirect_stdout(sys.stderr):
        return sync(full)


def main():
    cmd = sys.argv[1] if len(sys.argv) > 1 else "status"
    if cmd == "status":
        print(status())
    elif cmd == "route":
        print(route(" ".join(sys.argv[2:])))
    elif cmd in ("sync", "full"):
        print(quiet_sync(full=cmd == "full"))
    elif cmd == "start":
        result = quiet_sync()
        cp = CHECKPOINT.read_text("utf-8").split("\n\n")[1] if CHECKPOINT.exists() else ""
        context = (f"Knowledge graph sync at session start: {result}. graphify-out/graph.json is current; "
                   "query it before reading files (see CLAUDE.md, 'Knowledge graph').\n"
                   f"Last checkpoint:\n{cp}")
        print(json.dumps({"hookSpecificOutput": {"hookEventName": "SessionStart",
                                                 "additionalContext": context}}))
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:  # a hook must never break the session; say why and move on
        print(f"kg_sync: {type(exc).__name__}: {exc}", file=sys.stderr)
        sys.exit(1)
