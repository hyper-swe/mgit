import sys, lzma, re, json
status_path, pk_path, want = sys.argv[1], sys.argv[2], sys.argv[3:]
def stanzas(text):
    for blk in text.split('\n\n'):
        d, key = {}, None
        for line in blk.split('\n'):
            if line[:1] in ' \t' and key: d[key] += '\n' + line
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
for d in stanzas(lzma.open(pk_path, 'rt').read()):
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
for n in sorted(need):
    d = need[n]
    print(json.dumps({'package': n, 'version': d['Version'], 'file': d['Filename'], 'sha256': d['SHA256'], 'size': int(d['Size'])}))
