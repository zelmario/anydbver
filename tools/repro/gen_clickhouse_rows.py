#!/usr/bin/env python3
"""Generate clickhouse_version rows from packages.clickhouse.com metadata.

ClickHouse publishes one repository per channel, not per OS release: the
binaries are static, so the same packages serve EL 8/9/10 and every Debian or
Ubuntu release. The "stable" channel holds every release, "lts" only the LTS
series (X.3 and X.8). Rows always point at "stable" and the LTS list only feeds
version_series.

Every versioned package in a row is checked against the metadata of its
format (RPM primary.xml for EL rows, the apt Packages index for Debian rows).

Usage: python3 -I tools/repro/gen_clickhouse_rows.py > /tmp/clickhouse_rows.sql
"""
import gzip, re, sys, urllib.request

BASE = 'https://packages.clickhouse.com'
# Oldest series we ship rows for. LTS series go back about two years, regular
# monthly releases about six months: every patch of every series since 24.8
# would add 1.6 MB to anydbver_version.sql. Older releases stay installable by
# hand from the same repository.
MIN_LTS_SERIES = (25, 3)
MIN_SERIES = (26, 4)
EL = ['el8', 'el9', 'el10']
DEB = ['jammy', 'noble', 'bookworm']
PKGS = ['clickhouse-server', 'clickhouse-client', 'clickhouse-common-static']
ARCH = {'x86_64': 'amd64', 'aarch64': 'arm64'}


def key(v):
    return [int(x) for x in re.findall(r'\d+', v)]


def series(v):
    return '.'.join(v.split('.')[:2])


def get(u):
    # The CDN answers 403 to Python's default User-Agent.
    req = urllib.request.Request(u, headers={'User-Agent': 'curl/8.0'})
    return urllib.request.urlopen(req, timeout=120).read()


def rpm_versions(channel):
    md = get(f'{BASE}/rpm/{channel}/repodata/repomd.xml')
    href = re.search(rb'<data type="primary">.*?href="([^"]+)"', md, re.S).group(1).decode()
    x = gzip.decompress(get(f'{BASE}/rpm/{channel}/{href}')).decode()
    have = {}  # (version, arch) -> {package names}
    for p in re.findall(r'<package type="rpm">(.*?)</package>', x, re.S):
        name = re.search(r'<name>([^<]+)', p).group(1)
        arch = re.search(r'<arch>([^<]+)', p).group(1)
        ver = re.search(r'ver="([^"]+)"', p).group(1)
        if name in PKGS:
            have.setdefault((ver, arch), set()).add(name)
    return have


def deb_versions():
    have = {}
    for arch, debarch in ARCH.items():
        x = gzip.decompress(get(f'{BASE}/deb/dists/stable/main/binary-{debarch}/Packages.gz')).decode()
        for stanza in x.split('\n\n'):
            name = re.search(r'^Package: (.+)$', stanza, re.M)
            ver = re.search(r'^Version: (.+)$', stanza, re.M)
            if name and ver and name.group(1) in PKGS:
                have.setdefault((ver.group(1), arch), set()).add(name.group(1))
    return have


def q(v):
    return "'" + v.replace("'", "''") + "'"


rpm = rpm_versions('stable')
lts = {series(v) for (v, _a) in rpm_versions('lts')}


def wanted(v):
    if not re.fullmatch(r'\d+\.\d+\.\d+\.\d+', v):
        return False
    return tuple(key(v)[:2]) >= (MIN_LTS_SERIES if series(v) in lts else MIN_SERIES)

deb = deb_versions()

out = []
for (ver, arch), names in sorted(rpm.items(), key=lambda kv: (key(kv[0][0]), kv[0][1])):
    if arch not in ARCH or not wanted(ver) or set(PKGS) - names:
        continue
    pk = '|'.join(f'{n}-{ver}' for n in PKGS)
    for os_ in EL:
        out.append(f"INSERT INTO clickhouse_version VALUES({q(ver)},{q(os_)},{q(arch)},"
                   f"{q(BASE + '/rpm/stable/')},'/etc/yum.repos.d/clickhouse.repo','stable',"
                   f"'clickhouse-server','/etc/clickhouse-server/config.xml',{q(pk)},"
                   f"{q('clickhouse-common-static-dbg-' + ver)});")
for (ver, arch), names in sorted(deb.items(), key=lambda kv: (key(kv[0][0]), kv[0][1])):
    if not wanted(ver) or set(PKGS) - names:
        continue
    pk = '|'.join(f'{n}={ver}' for n in PKGS)
    for os_ in DEB:
        out.append(f"INSERT INTO clickhouse_version VALUES({q(ver)},{q(os_)},{q(arch)},"
                   f"{q(BASE + '/deb')},'/etc/apt/sources.list.d/clickhouse.list','stable',"
                   f"'clickhouse-server','/etc/clickhouse-server/config.xml',{q(pk)},"
                   f"{q('clickhouse-common-static-dbg=' + ver)});")

# ClickHouse supports the two newest LTS series and the three newest regular
# ones (https://clickhouse.com/docs/faq/operations/production).
all_series = sorted({series(v) for (v, _a) in rpm if wanted(v)}, key=key)
supported_lts = [s for s in all_series if s in lts][-2:]
supported_ga = [s for s in all_series if s not in lts][-3:]
for s in all_series:
    status = 'lts' if s in supported_lts else 'ga' if s in supported_ga else 'eol'
    out.append(f"INSERT INTO version_series VALUES('clickhouse',{q(s)},{q(status)});")

print('\n'.join(out))
counts = {}
for (v, a) in rpm:
    if wanted(v) and a in ARCH:
        counts[series(v)] = counts.get(series(v), 0) + 1
print('series (lts marked *):', ' '.join(f"{s}{'*' if s in lts else ''}={counts[s]}" for s in all_series), file=sys.stderr)
print('rows:', sum(1 for l in out if 'clickhouse_version' in l), file=sys.stderr)
