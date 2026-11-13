#!/usr/bin/env python3
"""Generate mongodb_version rows (MongoDB Community) from repo.mongodb.org yum metadata.

The repo index pages are rendered by JavaScript, so this reads
repodata/repomd.xml and primary.xml.gz the way dnf does.  Every versioned
package in a row is checked against that metadata.
"""
import re, gzip, sys, urllib.request, concurrent.futures as cf

MAJORS = ['5.0', '6.0', '7.0', '8.0', '8.2', '8.3', '9.0']
PKGS = ['mongodb-org-server', 'mongodb-org-mongos', 'mongodb-org-database', 'mongodb-org-database-tools-extra',
        'mongodb-org-tools', 'mongodb-org-shell', 'mongodb-org']
def key(v): return [int(x) for x in re.findall(r'\d+', v)]

def get(u):
    try: return urllib.request.urlopen(u, timeout=90).read()
    except Exception: return None

def scan(job):
    el, maj, arch = job
    base = f'https://repo.mongodb.org/yum/redhat/{el}/mongodb-org/{maj}/{arch}/'
    md = get(base + 'repodata/repomd.xml')
    if not md: return job, {}
    href = re.search(rb'<data type="primary">.*?href="([^"]+)"', md, re.S).group(1).decode()
    x = gzip.decompress(get(base + href)).decode()
    have = {}
    for p in re.findall(r'<package type="rpm">(.*?)</package>', x, re.S):
        name = re.search(r'<name>([^<]+)', p).group(1)
        ver, rel = re.search(r'ver="([^"]+)" rel="([^"]+)"', p).groups()
        if name in PKGS: have.setdefault(f'{ver}-{rel}', set()).add(name)
    return job, have

def q(v): return "'" + v.replace("'", "''") + "'"
jobs = [(el, m, a) for el in (8, 9, 10) for m in MAJORS for a in ('x86_64', 'aarch64')]
out = []
with cf.ThreadPoolExecutor(16) as ex:
    for (el, maj, arch), have in ex.map(scan, jobs):
        for vr in sorted(have, key=key):
            if not re.fullmatch(r'\d+\.\d+\.\d+-\d+\.el\d+', vr) or 'mongodb-org-server' not in have[vr]: continue  # skip rc/alpha builds
            version = vr.rsplit('.', 1)[0]           # 8.0.32-1
            pk = [f'{n}-{vr}.{arch}' for n in PKGS if n in have[vr]] + ['mongodb-mongosh', 'mongodb-database-tools']
            out.append('INSERT INTO mongodb_version VALUES(%s);' % ','.join(q(v) for v in [
                version, f'el{el}', arch,
                f'https://repo.mongodb.org/yum/redhat/{el}/mongodb-org/{maj}/{arch}/',
                f'/etc/yum.repos.d/mongodb-org-{maj}.repo', '', 'mongod', '/etc/mongod.conf',
                '|'.join(pk), 'gdb']))
print('\n'.join(out))
print(len(out), 'rows', file=sys.stderr)
