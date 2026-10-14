import sys, lzma, re, json
args = sys.argv[1:]
req_out = None
lock_only = None
if args and args[0] == '--requires-from-lock':
    # <lock> <Packages> <out>: recompute only the base requirements of the
    # packages already pinned in the lock; no closure, no base status needed
    lock_only, args = args[1], args[2:]
if args and args[0] == '--requires-out':
    req_out, args = args[1], args[2:]
if lock_only:
    pk_path, req_out = args[0], args[1]
    status_path, want = '/dev/null', []
else:
    status_path, pk_path, want = args[0], args[1], args[2:]
def stanzas(text):
    for blk in text.split('\n\n'):
        d, key = {}, None
        for line in blk.split('\n'):
            if line and line[0] in ' \t' and key: d[key] += '\n' + line
            elif ':' in line:
                key, v = line.split(':', 1); d[key] = v.strip()
        if 'Package' in d: yield d
inst, provided = set(), set()
for d in stanzas(open(status_path).read()):
    if 'installed' in d.get('Status', '') and 'not-installed' not in d['Status']:
        inst.add(d['Package'])
        for p in d.get('Provides', '').split(','):
            if p.strip(): provided.add(p.strip().split()[0])
inst |= provided
pk = {}
prov = {}
_raw = open(pk_path, 'rb').read()
if _raw[:6] == b'\xfd7zXZ\x00':
    _raw = lzma.decompress(_raw)
for d in stanzas(_raw.decode()):
    pk[d['Package']] = d
    for p in d.get('Provides', '').split(','):
        if p.strip(): prov.setdefault(p.strip().split()[0], d['Package'])
def deps(d):
    out = []
    for f in ('Pre-Depends', 'Depends'):
        for grp in d.get(f, '').split(','):
            alts = [re.split(r'[ (:]', a.strip())[0] for a in grp.split('|') if a.strip()]
            if alts: out.append(alts)
    return out
need, queue = {}, list(want)
if lock_only:
    names = [l.split()[0] for l in open(lock_only) if l.strip() and not l.startswith('#')]
    for n in names:
        if n in pk: need[n] = pk[n]
    queue = []
while queue:
    n = queue.pop()
    if n in need or n in inst: continue
    if n not in pk: n = prov.get(n, n)
    if n not in pk: print('UNRESOLVED', n, file=sys.stderr); continue
    if n in need or n in inst: continue
    need[n] = pk[n]
    for alts in deps(pk[n]):
        if any(a in inst or a in need for a in alts): continue
        queue.append(alts[0])
for n in ([] if lock_only else sorted(need)):
    d = need[n]
    print(json.dumps({'package': n, 'version': d['Version'], 'file': d['Filename'], 'sha256': d['SHA256'], 'size': int(d['Size']), 'origin': d.get('X-Origin', 'main')}))

if req_out:
    # exact-version dependencies the pinned packages place on packages the base
    # image supplies (for example perl needs perl-base = X); checked against the
    # base by guest-debs-check.py base
    req = {}
    for n, d in need.items():
        for grp in d.get('Depends', '').split(','):
            m = re.match(r'\s*([^\s(|]+)\s*\(= ([^)]+)\)\s*$', grp)
            if m and m.group(1) not in need:
                req[m.group(1)] = m.group(2)
    with open(req_out, 'w') as f:
        f.write('# <package> <exact version> required of the base image by the pinned packages\n')
        for k in sorted(req):
            f.write(f'{k} {req[k]}\n')
