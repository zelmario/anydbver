#!/usr/bin/env python3
"""New PostgreSQL rows for percona_postgresql_version and postgresql_version.

Usage: python3 -I gen_pg_rows.py anydbver_version.db new.sql

For every (major, os, arch) that already has rows, the newest row is the
template. The newest upstream build of that major is looked up in the real
repository (Percona ppg-N yum, PGDG yum, PGDG apt Packages), and every
versioned package token of the template is replaced by the matching file
from that listing, so a row never names a package that does not exist.
Only builds newer than the template are emitted. A log goes to new.sql.log.
"""
import gzip, re, sqlite3, sys, urllib.request
from functools import lru_cache

DB, OUT = sys.argv[1], sys.argv[2]
LOG = open(OUT + '.log', 'w')
def log(m): print(m, file=LOG)

@lru_cache(None)
def fetch(url, binary=False):
    try:
        data = urllib.request.urlopen(urllib.request.Request(url, headers={'User-Agent': 'curl'}), timeout=90).read()
    except Exception as e:
        log(f'fetch failed {url}: {e}')
        return b'' if binary else ''
    return data if binary else data.decode('utf-8', 'replace')

def vkey(v):
    # (0, n) sorts numbers, (1, s) text, so mixed parts never compare int to str.
    # Debian's "~" sorts before anything: 17~rc1 is older than 17.0.
    v = v.replace('~', '.~.')
    return [(-1, '') if x == '~' else (0, int(x)) if x.isdigit() else (1, x)
            for x in re.split(r'[.\-+]|(?<=\d)(?=[A-Za-z])|(?<=[A-Za-z])(?=\d)', v) if x != '']

def short_rel(rel):
    # Rows store the release without the dist: 18.6-1, not 18.6-1.el9 or
    # 18.6-1PGDG.rhel9.8, so ppg:18.7 and pg:18.7 prefix matches keep working.
    return re.sub(r'(PGDG.*|\.el\d+.*)$', '', rel)

RPM = re.compile(r'^(?P<name>.+?)-(?P<ver>\d[\w.]*)-(?P<rel>[\w.]+?)(?:\.(?P<arch>x86_64|aarch64|noarch))?$')

def rpms(url):
    """{name: [(ver, rel, filename_without_rpm)]}"""
    out = {}
    for f in set(re.findall(r'href="(?:[^"]*/)?([^"/]+)\.rpm"', fetch(url))):
        m = RPM.match(f)
        if m:
            out.setdefault(m['name'], []).append((m['ver'], m['rel'], f))
    return out

def elnum(os): return os[2:]

con = sqlite3.connect(DB)
con.row_factory = sqlite3.Row
out = open(OUT, 'w')

def emit(table, row, cols):
    vals = ','.join("'" + str(row[c]).replace("'", "''") + "'" for c in cols)
    out.write(f'INSERT INTO {table} VALUES({vals});\n')

def major(v): return v.split('.')[0]

def el_rows(table, listing_url, server_name):
    cols = [d[1] for d in con.execute(f'pragma table_info({table})')]
    rows = [dict(r) for r in con.execute(f"select * from {table} where os like 'el%'")]
    have = {(r['version'], r['os'], r['arch']) for r in rows}
    groups = {}
    for r in rows:
        if not major(r['version']).isdigit() or int(major(r['version'])) < 13:
            continue
        groups.setdefault((major(r['version']), r['os'], r['arch']), []).append(r)
    for (maj, os, arch), rs in sorted(groups.items()):
        tpl = max(rs, key=lambda r: vkey(r['version']))
        lst = rpms(listing_url(maj, elnum(os), arch))
        server = server_name(maj)
        builds = sorted({(v, rl) for v, rl, f in lst.get(server, [])}, key=lambda b: vkey(b[0] + '-' + b[1]))
        if not builds:
            log(f'{table} {maj} {os}/{arch}: no {server} in listing')
            continue
        ver, rel = builds[-1]
        new_version = f'{ver}-{short_rel(rel)}'
        if vkey(new_version) <= vkey(tpl['version']) or (new_version, os, arch) in have:
            continue
        r = dict(tpl)
        r['version'] = new_version
        ok = True
        for col in ('packages', 'debug_packages'):
            toks = []
            for t in (tpl[col] or '').split('|'):
                if not t:
                    continue
                m = RPM.match(t)
                if not m or m['name'] not in lst:
                    toks.append(t)  # gdb and other unversioned names
                    continue
                files = [f for v, rl, f in lst[m['name']] if v == ver and rl == rel]
                if not files:
                    if col == 'debug_packages':
                        log(f'{table} {new_version} {os}/{arch}: dropped {m["name"]} from debug_packages')
                        continue
                    log(f'SKIP {table} {new_version} {os}/{arch}: {m["name"]} {ver}-{rel} missing')
                    ok = False
                    break
                f = files[0]
                # keep the template's style: with or without the arch suffix
                toks.append(f if m['arch'] else re.sub(r'\.(x86_64|aarch64|noarch)$', '', f))
            if not ok:
                break
            r[col] = '|'.join(toks)
        if ok:
            emit(table, r, cols)
            log(f'{table} {new_version} {os}/{arch} from {tpl["version"]}')

el_rows('percona_postgresql_version',
        lambda maj, el, arch: f'https://repo.percona.com/ppg-{maj}/yum/release/{el}/RPMS/{arch}/',
        lambda maj: f'percona-postgresql{maj}-server')
el_rows('postgresql_version',
        lambda maj, el, arch: f'https://download.postgresql.org/pub/repos/yum/{maj}/redhat/rhel-{el}-{arch}/',
        lambda maj: f'postgresql{maj}-server')

# PGDG apt: the version string is the Debian package version.
cols = [d[1] for d in con.execute('pragma table_info(postgresql_version)')]
rows = [dict(r) for r in con.execute("select * from postgresql_version where os not like 'el%'")]
have = {(r['version'], r['os'], r['arch']) for r in rows}
groups = {}
for r in rows:
    m = re.search(r'postgresql-(\d+)=', r['packages'])
    if m and int(m.group(1)) >= 13:
        groups.setdefault((m.group(1), r['os'], r['arch']), []).append(r)
for (maj, os, arch), rs in sorted(groups.items()):
    tpl = max(rs, key=lambda r: vkey(r['version']))
    darch = 'amd64' if arch == 'x86_64' else 'arm64'
    pkgs = fetch(f'https://apt.postgresql.org/pub/repos/apt/dists/{os}-pgdg/main/binary-{darch}/Packages.gz', binary=True)
    try:
        text = gzip.decompress(pkgs).decode('utf-8', 'replace')
    except Exception:
        log(f'postgresql_version {maj} {os}/{arch}: no Packages.gz')
        continue
    vers = []
    for stanza in text.split('\n\n'):
        f = dict(re.findall(r'^([\w-]+): (.*)$', stanza, re.M))
        if f.get('Package') == f'postgresql-{maj}':
            vers.append(f['Version'])
    if not vers:
        log(f'postgresql_version {maj} {os}/{arch}: postgresql-{maj} not in Packages')
        continue
    ver = max(vers, key=vkey)
    if vkey(ver) <= vkey(tpl['version']) or (ver, os, arch) in have:
        continue
    r = dict(tpl)
    r['version'] = ver
    r['packages'] = tpl['packages'].replace(tpl['version'], ver)
    r['debug_packages'] = (tpl['debug_packages'] or '').replace(tpl['version'], ver)
    emit('postgresql_version', r, cols)
    log(f'postgresql_version {ver} {os}/{arch} from {tpl["version"]}')

out.close()
LOG.close()
