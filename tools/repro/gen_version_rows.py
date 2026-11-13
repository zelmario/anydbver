#!/usr/bin/env python3
"""Generate missing version rows for anydbver_version.sql from live repo listings.

Every versioned package in a generated row is resolved against the actual
repository listing, never string-substituted and trusted.  Writes new.sql
(INSERT lines grouped per table) and gen.log (every fallback and drop).
"""
import re, sqlite3, sys, json, urllib.request, concurrent.futures as cf
from functools import lru_cache

DB = sys.argv[1]
OUT = sys.argv[2]
LOG = open(OUT + '.log', 'w')
def log(*a): print(*a, file=LOG)

def key(v): return [int(x) for x in re.findall(r'\d+', v)]

@lru_cache(None)
def fetch(u):
    for _ in range(3):
        try:
            return urllib.request.urlopen(urllib.request.Request(u, headers={'User-Agent': 'curl'}), timeout=90).read().decode('utf-8', 'replace')
        except urllib.error.HTTPError as e:
            if e.code in (403, 404, 410): return ''
        except Exception:
            pass
    return ''

@lru_cache(None)
def exists(u):
    try:
        r = urllib.request.urlopen(urllib.request.Request(u, method='HEAD', headers={'User-Agent': 'curl'}), timeout=60)
        return r.status == 200
    except Exception:
        return False

RPM = re.compile(r'^(?P<name>.+)-(?P<ver>\d[^-]*)-(?P<rel>[^-]+?)\.(?P<dist>el\d+(?:_\d+)?)\.(?P<arch>x86_64|aarch64|noarch)$')
def parse(tok):
    m = RPM.match(tok)
    return m.groupdict() if m else None

@lru_cache(None)
def listing(url):
    """{name: [(ver, rel, dist, arch, filename_without_rpm)]} for one repo directory."""
    out = {}
    for f in set(re.findall(r'href="(?:[^"]*/)?([^"/]+)\.rpm"', fetch(url), re.I)):
        p = parse(f)
        if p: out.setdefault(p['name'], []).append((p['ver'], p['rel'], p['dist'], p['arch'], f))
    return out

def elnum(os): return os[2:]

def resolve(tok, ver, rel, lst, why):
    """Pick the build of tok's package for (ver, rel) from listing lst."""
    p = parse(tok)
    if not p: return tok
    c = [x for x in lst.get(p['name'], []) if x[3] == p['arch'] and x[2].startswith(p['dist'][:4].rstrip('_'))]
    exact = [x for x in c if x[0] == ver and x[1] == rel]
    if exact: return exact[0][4]
    same = sorted([x for x in c if x[0] == ver], key=lambda x: key(x[1]))
    if same:
        log(f'  {why}: {p["name"]} no {ver}-{rel}, using release {same[-1][1]}')
        return same[-1][4]
    older = sorted([x for x in c if key(x[0]) <= key(ver)], key=lambda x: key(x[0] + '-' + x[1]))
    if older:
        log(f'  {why}: {p["name"]} has no {ver} build, falling back to {older[-1][0]}-{older[-1][1]}')
        return older[-1][4]
    return None

def sqlq(v): return "'" + str(v).replace("'", "''") + "'"
NEW = {}
def emit(table, row):
    NEW.setdefault(table, []).append('INSERT INTO %s VALUES(%s);' % (table, ','.join(sqlq(x) for x in row)))

db = sqlite3.connect(DB)
db.row_factory = sqlite3.Row
def rows(table): return [dict(r) for r in db.execute(f'select * from {table}')]
def have(table): return {(r['version'], r['os'], r['arch']) for r in rows(table)}

def retarget(row, os, arch):
    """Template from another os/arch: rewrite os and arch tokens."""
    r = dict(row)
    for k, v in r.items():
        if k == 'version' or not isinstance(v, str): continue
        v = re.sub(r'\.' + row['os'] + r'\.', '.' + os + '.', v)
        v = v.replace('/' + elnum(row['os']) + '/', '/' + elnum(os) + '/')
        if arch != row['arch']: v = v.replace(row['arch'], arch)
        r[k] = v
    r['os'], r['arch'] = os, arch
    return r

COLS_OPTIONAL = {'debug_packages', 'rocksdb_packages', 'tests_packages', 'mysql_shell_packages', 'mysql_router_packages', 'garbd_packages'}

def build_rpm_row(table, tpl, ver, rel, lst, extra_lists=()):
    """Resolve every versioned token of the template against the listing."""
    r = dict(tpl)
    r['version'] = f'{ver}-{rel}'
    why = f'{table} {r["version"]} {r["os"]}/{r["arch"]}'
    for k, v in tpl.items():
        if k in ('version', 'os', 'arch') or not isinstance(v, str) or '|' not in v and not parse(v): continue
        out = []
        for t in v.split('|'):
            if not parse(t): out.append(t); continue
            n = resolve(t, ver, rel, lst, why)
            if k == 'debug_packages' and n and parse(n)['ver'] != ver:
                log(f'  {why}: dropped {parse(t)["name"]} from {k}, no {ver} build (would conflict)'); continue
            for el in extra_lists:
                if n is None: n = resolve(t, ver, rel, el, why)
            if n is None:
                if k in COLS_OPTIONAL:
                    log(f'  {why}: dropped {parse(t)["name"]} from {k}, not in repo'); continue
                log(f'SKIP {why}: required {parse(t)["name"]} missing'); return None
            out.append(n)
        r[k] = '|'.join(out)
    return r

# ---------- Percona yum products ----------
PERCONA = [
    # table, server package, {major: repo}, gap combos to fill with latest only
    ('percona_server_version', 'percona-server-server', {'8.0': 'ps-80', '8.4': 'ps-84-lts', '9.7': 'ps-97-lts'}),
    ('percona_xtradb_cluster_version', 'percona-xtradb-cluster-server', {'8.0': 'pxc-80', '8.4': 'pxc-84-lts', '9.7': 'pxc-97-lts'}),
    ('percona_server_mongodb_version', 'percona-server-mongodb-server', {'7.0': 'psmdb-70', '8.0': 'psmdb-80', '8.3': 'psmdb-83'}),
    ('percona_xtrabackup_version', None, {'8.0': 'pxb-80', '8.4': 'pxb-84-lts'}),
]
COMBOS = [(f'el{e}', a) for e in (8, 9, 10) for a in ('x86_64', 'aarch64')]
def prepo(repo, os, arch): return f'https://repo.percona.com/{repo}/yum/release/{elnum(os)}/RPMS/{arch}/'

# prefetch all listings in parallel
urls = [prepo(repo, os, a) for _, _, m in PERCONA for repo in m.values() for os, a in COMBOS]
urls += [prepo('pbm', os, a) for os, a in COMBOS] + [prepo('pdps-8.0', os, 'x86_64') for os in ('el8', 'el9')]
with cf.ThreadPoolExecutor(24) as ex: list(ex.map(fetch, urls))

def major_of(v): return '.'.join(v.split('.')[:2])

for table, server, majors in PERCONA:
    existing = rows(table)
    got = have(table)
    for maj, repo in majors.items():
        srv = server or f'percona-xtrabackup-{maj.replace(".", "")}'
        mine = [r for r in existing if major_of(r['version']) == maj]
        if not mine:  # new series: template from the previous series
            prev = sorted({major_of(r['version']) for r in existing}, key=key)
            prev = [m for m in prev if m in majors and key(m) < key(maj)][-1]
            old_repo = majors[prev]
            mine = []
            for r in existing:
                if major_of(r['version']) != prev: continue
                r = dict(r)
                for k in ('repo_file', 'repo_enable_str'):
                    r[k] = r[k].replace(old_repo, repo)
                mine.append(r)
            log(f'{table} {maj}: new series, templates from {prev}')
        for os, arch in COMBOS:
            lst = listing(prepo(repo, os, arch))
            builds = sorted({(x[0], x[1]) for x in lst.get(srv, []) if x[3] == arch and 'rc' not in x[1]}, key=lambda b: key(b[0] + '-' + b[1]))
            if not builds: continue
            same = sorted([r for r in mine if r['os'] == os and r['arch'] == arch], key=lambda r: key(r['version']))
            if same:
                tpl, top = same[-1], key(same[-1]['version'])
                want = [b for b in builds if key(b[0] + '-' + b[1]) > top]
            else:  # coverage gap: latest build only, template retargeted from a sibling combo
                sib = sorted(mine, key=lambda r: (r['arch'] == arch, r['os'] == 'el9', key(r['version'])))
                tpl = retarget(sib[-1], os, arch)
                want = [builds[-1]]
                log(f'{table} {maj} {os}/{arch}: coverage gap, adding latest only, template {sib[-1]["version"]} {sib[-1]["os"]}/{sib[-1]["arch"]}')
            for ver, rel in want:
                if (f'{ver}-{rel}', os, arch) in got: continue
                r = build_rpm_row(table, tpl, ver, rel, lst)
                if r: emit(table, [r[c] for c in tpl.keys()]); got.add((r['version'], os, arch))

# ---------- PBM ----------
existing = rows('percona_backup_mongodb_version'); got = have('percona_backup_mongodb_version')
for os, arch in COMBOS:
    lst = listing(prepo('pbm', os, arch))
    builds = sorted({(x[0], x[1]) for x in lst.get('percona-backup-mongodb', []) if x[3] == arch}, key=lambda b: key(b[0] + '-' + b[1]))
    if not builds: continue
    mine = [r for r in existing if r['os'] == os and r['arch'] == arch]
    top = max((key(r['version']) for r in mine), default=None)
    want = [b for b in builds if top is not None and key(b[0] + '-' + b[1]) > top] if mine else [builds[-1]]
    for ver, rel in want:
        if (f'{ver}-{rel}', os, arch) in got: continue
        f = [x[4] for x in lst['percona-backup-mongodb'] if x[0] == ver and x[1] == rel and x[3] == arch][0]
        emit('percona_backup_mongodb_version', [f'{ver}-{rel}', os, arch, prepo('pbm', os, arch) + f + '.rpm'])

# ---------- Orchestrator (general_version, role enables pdps-8.0) ----------
gv = {(r['version'], r['os'], r['arch']) for r in rows('general_version') if r['program'] == 'percona-orchestrator'}
for os in ('el8', 'el9'):
    lst = listing(prepo('pdps-8.0', os, 'x86_64'))
    mine = [key(v) for v, o, a in gv if o == os]
    for b in sorted({(x[0], x[1]) for x in lst.get('percona-orchestrator', []) if x[3] == 'x86_64'}, key=lambda b: key(b[0] + '-' + b[1])):
        v = f'{b[0]}-{b[1]}'
        if mine and key(v) > max(mine) and (v, os, 'x86_64') not in gv:
            emit('general_version', [v, os, 'x86_64', 'percona-orchestrator'])

# ---------- MySQL (Oracle) ----------
MYSQL_TOOLS = {'8.0': 'mysql-tools-community', '8.4': 'mysql-tools-8.4-community', '9.7': 'mysql-tools-9.7-community', '26.7': 'mysql-tools-innovation-community'}
# Release rpm and repo id per stream. 8.0 keeps its default repo, the
# others are switched on explicitly by roles/mysql.
MYSQL_RELEASE = {'8.0': 'mysql80', '8.4': 'mysql84', '9.7': 'mysql97', '26.7': 'mysql80'}
MYSQL_ENABLE = {'8.0': '', '8.4': 'mysql-8.4-lts-community', '9.7': 'mysql-9.7-lts-community', '26.7': 'mysql-innovation-community'}
MYSQL = {'8.0': 'mysql-8.0-community', '8.4': 'mysql-8.4-community', '9.7': 'mysql-9.7-community', '26.7': 'mysql-innovation-community'}
existing = rows('mysql_server_version'); got = have('mysql_server_version')
def mysql_url_tok(t, old, new, why):
    if not t.startswith('http'): return t
    n = t.replace(old, new)
    cands = [n]
    fn = n.rsplit('/', 1)[1]
    if 'mysql-shell' in fn:
        cands += [f'https://cdn.mysql.com/archives/mysql-shell/{fn}', f'https://cdn.mysql.com/Downloads/MySQL-Shell/{fn}']
    else:
        mj = major_of(new)
        cands += [f'https://cdn.mysql.com/archives/mysql-{mj}/{fn}', f'https://cdn.mysql.com/Downloads/MySQL-{mj}/{fn}']
    for c in cands:
        if exists(c): return c
    if 'mysql-shell' in fn:
        a, b, c3 = (int(x) for x in new.split('.'))
        for pv in range(c3 - 1, max(c3 - 4, -1), -1):
            fb = fn.replace(new, f'{a}.{b}.{pv}')
            for base in ('https://cdn.mysql.com/archives/mysql-shell/', 'https://cdn.mysql.com/Downloads/MySQL-Shell/'):
                if exists(base + fb):
                    log(f'  {why}: no {fn} on cdn, falling back to {fb}'); return base + fb
    log(f'  {why}: {fn} not found on cdn.mysql.com')
    return None
for maj, repo in MYSQL.items():
    for os, arch in COMBOS:
        mine = sorted([r for r in existing if major_of(r['version']) == maj and r['os'] == os and r['arch'] == arch], key=lambda r: key(r['version']))
        if not mine: continue  # no el10 rows yet: the release rpm path is untested there
        lst = listing(f'https://repo.mysql.com/yum/{repo}/el/{elnum(os)}/{arch}/')
        builds = sorted({(x[0], x[1]) for x in lst.get('mysql-community-server', []) if x[3] == arch}, key=lambda b: key(b[0] + '-' + b[1]))
        tpl = mine[-1]
        oldv = tpl['version'].split('-')[0]
        for ver, rel in builds:
            if key(f'{ver}-{rel}') <= key(tpl['version']) or (f'{ver}-{rel}', os, arch) in got: continue
            why = f'mysql {ver}-{rel} {os}/{arch}'
            tools = listing(f'https://repo.mysql.com/yum/{MYSQL_TOOLS[maj]}/el/{elnum(os)}/{arch}/')
            # 9.7 and innovation rows list packages without the arch suffix,
            # which parse() needs, so their versions were copied unchanged:
            # the 9.7.4 row installed 9.7.2. Add the arch before resolving.
            def with_arch(v):
                return '|'.join(t + '.' + arch if not parse(t) and re.match(r'mysql-community-[a-z-]+-[\d.]+-\d+\.el\d+$', t) else t for t in v.split('|'))
            tpl_in = {k: ('|'.join(x for x in v.split('|') if not x.startswith('http')) if isinstance(v, str) and k != 'repo_url' else v) for k, v in tpl.items()}
            tpl_in['packages'] = with_arch(tpl_in['packages'])
            r = build_rpm_row('mysql_server_version', tpl_in, ver, rel, lst, (tools,))
            if not r: continue
            for k in ('debug_packages', 'mysql_shell_packages'):
                toks = [mysql_url_tok(t, oldv, ver, why) for t in tpl[k].split('|') if t]
                r[k] = '|'.join(t for t in toks if t)
            # The test package is a plain name, not a URL, so mysql_url_tok keeps
            # the template's version: 8.4.11 rows asked for the 8.4.9 test rpm.
            r['debug_packages'] = re.sub(r'mysql-community-test-[\d.]+-\d+\.', f'mysql-community-test-{ver}-{rel}.', r['debug_packages'])
            # The template row can come from another stream (8.4.7+ rows got the
            # 8.0 release rpm), so set the repo from the major, issue #10.
            if maj in MYSQL_RELEASE:
                r['repo_url'] = f'http://repo.mysql.com/{MYSQL_RELEASE[maj]}-community-release-el{elnum(os)}.rpm'
                r['repo_enable_str'] = MYSQL_ENABLE[maj]
            emit('mysql_server_version', [r[c] for c in tpl.keys()]); got.add((r['version'], os, arch))

# ---------- MariaDB (archive.mariadb.org) ----------
existing = rows('mariadb_version'); got = have('mariadb_version')
arch_dirs = re.findall(r'href="mariadb-(\d+\.\d+\.\d+)/"', fetch('https://archive.mariadb.org/'))
upstream = {}
for v in arch_dirs: upstream.setdefault(major_of(v), set()).add(v)
MARIADB_MAJ = ['10.6', '10.11', '11.4', '11.8', '12.3', '13.0', '13.1']
def mdir(v, os, arch): return f'https://archive.mariadb.org/mariadb-{v}/yum/rhel{elnum(os)}-{"amd64" if arch == "x86_64" else "aarch64"}/rpms/'
for maj in MARIADB_MAJ:
    mine = [r for r in existing if major_of(r['version']) == maj]
    if not mine:
        mine = [r for r in existing if major_of(r['version']) == '12.3']
        log(f'mariadb {maj}: new series, templates from 12.3')
    vers = sorted(upstream.get(maj, []), key=key)
    for os, arch in [('el8', 'x86_64'), ('el8', 'aarch64'), ('el9', 'x86_64'), ('el9', 'aarch64')]:
        same = sorted([r for r in mine if r['os'] == os and r['arch'] == arch], key=lambda r: key(r['version']))
        if same and major_of(same[-1]['version']) == maj:
            tpl = same[-1]; want = [v for v in vers if key(v) > key(tpl['version'].split('-')[0])]
        else:
            sib = sorted([r for r in mine if r['arch'] == arch], key=lambda r: key(r['version']))
            if not sib: continue
            tpl = retarget(sib[-1], os, arch) if sib[-1]['os'] != os else sib[-1]
            want = vers[-1:] if (major_of(sib[-1]['version']) != maj or sib[-1]['os'] != os) else []
            if want: log(f'mariadb {maj} {os}/{arch}: {"new series" if major_of(sib[-1]["version"]) != maj else "coverage gap"}, latest only, template {sib[-1]["version"]} {sib[-1]["os"]}')
        oldv = tpl['version'].split('-')[0]
        for v in want:
            lst = listing(mdir(v, os, arch))
            if not lst: continue
            srv = [x for x in lst.get('MariaDB-server', []) if x[0] == v and x[3] == arch]
            if not srv: continue
            rel = srv[0][1]
            if (f'{v}-{rel}', os, arch) in got: continue
            why = f'mariadb {v} {os}/{arch}'
            r = dict(tpl); r['version'] = f'{v}-{rel}'; ok = True
            names = {x[4] for l in lst.values() for x in l}
            for k in ('packages', 'debug_packages', 'rocksdb_packages', 'tests_packages', 'mysql_shell_packages'):
                out = []
                for t in tpl[k].split('|'):
                    if not t.startswith('http'): out.append(t) if t else None; continue
                    fn = t.rsplit('/', 1)[1][:-4]
                    p = parse(fn)
                    n = resolve(fn, v, rel, lst, why) if p else None
                    if n: out.append(mdir(v, os, arch) + n + '.rpm')
                    elif k == 'packages': log(f'SKIP {why}: required {fn} missing'); ok = False
                    else: log(f'  {why}: dropped {p["name"] if p else fn} from {k}')
                r[k] = '|'.join(out)
            if ok: emit('mariadb_version', [r[c] for c in tpl.keys()]); got.add((r['version'], os, arch))

# ---------- k8s operators ----------
K8S = ['percona/percona-postgresql-operator', 'percona/percona-server-mongodb-operator', 'percona/percona-server-mysql-operator',
       'percona/percona-xtradb-cluster-operator', 'cloudnative-pg/cloudnative-pg']
kv = {(r['name'], r['version']) for r in rows('k8s_operators_version')}
for n in K8S:
    rel = json.loads(fetch(f'https://api.github.com/repos/{n}/releases?per_page=30') or '[]')
    mine = sorted([v for nn, v in kv if nn == n], key=key)
    for r in rel:
        v = r['tag_name'].lstrip('v')
        if r['prerelease'] or r['draft'] or not re.fullmatch(r'\d+\.\d+\.\d+', v): continue
        # patch releases of any minor we already track, plus anything newer than our newest
        if (n, v) not in kv and (key(v) > key(mine[-1]) or major_of(v) in {major_of(x) for x in mine}) and key(v) > key(min(mine, key=key)):
            if any(major_of(x) == major_of(v) and key(x) > key(v) for x in mine): continue
            emit('k8s_operators_version', [n, v])

with open(OUT, 'w') as f:
    for t, l in NEW.items():
        f.write(f'-- {t}: {len(l)}\n' + '\n'.join(l) + '\n')
for t, l in NEW.items(): print(t, len(l))
