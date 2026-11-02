# Mutation check (CLAUDE.md rule 4: prove the test can fail). Run from engine/:
# python mutate.py <pkg dir> <test regex> <file> <old> <new> [<file> <old> <new> ...]
# Applies each mutation alone, runs the tests, reports whether they failed, restores.
import subprocess, sys
pkg, run = sys.argv[1], sys.argv[2]
args = sys.argv[3:]
for i in range(0, len(args), 3):
    f, old, new = args[i:i + 3]
    src = open(f, encoding='utf-8').read()
    n = src.count(old)
    if n != 1:
        print(f'SKIP ({n} matches) {old[:60]!r}')
        continue
    open(f, 'w', encoding='utf-8', newline='\n').write(src.replace(old, new))
    try:
        r = subprocess.run(['go', 'test', '-race', '-count=1', '-run', run, pkg], capture_output=True, text=True)
        out = r.stdout + r.stderr
        verdict = 'CAUGHT' if r.returncode != 0 and 'FAIL:' in out else ('BUILD-ERR' if r.returncode != 0 else 'MISSED')
        print(f'{verdict:9} {old[:70]!r}')
    finally:
        open(f, 'w', encoding='utf-8', newline='\n').write(src)
