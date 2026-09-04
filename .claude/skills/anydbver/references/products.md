# Products and aliases

> Verified on 2026-09-04 against `anydbver_version.sql` and `anydbver deploy help keywords`. Run that command for the live, authoritative list.
>
> To discover deployable versions, prefer **`anydbver versions [software]`** (added in v0.1.37): `anydbver versions` for an overview, `anydbver versions psmdb` for the full per-major list, plus `--latest`, `--os`, `--arch`, `--all`, `--json`.

## Databases

| Keyword                   | Aliases                       | What it is                                |
|---------------------------|-------------------------------|-------------------------------------------|
| `percona-server`          | `ps`, `percona-server-mysql`  | Percona Server for MySQL                  |
| `percona-xtradb-cluster`  | `pxc`                         | Percona XtraDB Cluster (Galera)           |
| `mysql`                   |                               | Oracle MySQL Community                    |
| `mariadb`                 |                               | MariaDB                                   |
| `mydb`                    |                               | MySQL-family variant (same options as `mysql`) |
| `postgresql`              | `pg`, `postgres`              | PostgreSQL from PGDG                      |
| `percona-postgresql`      | `ppg`, `percona-postgres`     | Percona Distribution for PostgreSQL       |
| `percona-server-mongodb`  | `psmdb`                       | Percona Server for MongoDB                |
| `valkey`                  |                               | Valkey (Redis fork)                       |

## Replication / HA / proxies

| Keyword                  | What it is                                       |
|--------------------------|--------------------------------------------------|
| `patroni`                | PostgreSQL HA (etcd + auto-failover)             |
| `repmgr`                 | PostgreSQL replication manager                   |
| `percona-orchestrator`   | MySQL topology manager                           |
| `percona-proxysql`       | ProxySQL                                         |
| `haproxy-pg`             | HAProxy tuned for PostgreSQL                     |
| `haproxy-patroni`        | HAProxy tuned for Patroni clusters               |
| `pgbouncer`              | PostgreSQL connection pooler                     |

## Backup

| Keyword                                  | Alias  | What it is                                |
|------------------------------------------|--------|-------------------------------------------|
| `pgbackrest`                             |        | PostgreSQL backup tool                    |
| `barman`                                 |        | PostgreSQL backup manager                 |
| `percona-backup-mongodb`                 | `pbm`  | MongoDB backup tool                       |
| `percona-xtrabackup`                     |        | Hot backup tool for MySQL / Percona Server |

## Monitoring / Observability

| Keyword              | Aliases | What it is                              |
|----------------------|---------|-----------------------------------------|
| `pmm-server`         | `pmm`   | Percona Monitoring and Management server |
| `pmm-client`         |         | Percona Monitoring and Management agent |
| `coroot-server`      | `coroot` | Coroot observability server (open-source PMM alternative) |
| `coroot-client`      | `coroot-agent` | Registers a database with a Coroot server; installs nothing on the node |
| `k8s-pmm`            |         | PMM running inside Kubernetes           |

## Object storage

| Keyword       | What it is                             |
|---------------|----------------------------------------|
| `minio`       | MinIO S3-compatible storage            |
| `k8s-minio`   | MinIO inside Kubernetes                |

## Shared filesystem

| Keyword        | Aliases | What it is                                          |
|----------------|---------|-----------------------------------------------------|
| `nfs-server`   | `nfs`   | NFSv4 server exporting `/srv/nfs` (tmpfs-backed). New in v0.1.33. |
| `nfs-client`   |         | Mount the export at `/mnt/nfs` (or `mount=/path`)   |

## Auth / directory

| Keyword        | What it is                                 |
|----------------|--------------------------------------------|
| `ldap`         | OpenLDAP server                            |
| `ldap-master`  | Point a client node at an LDAP server      |
| `kerberos`     | Kerberos (Samba-based) KDC                 |

## Kubernetes

| Keyword                          | Aliases                  | What it is                           |
|----------------------------------|--------------------------|--------------------------------------|
| `k3d`                            |                          | Create a nested k3d cluster          |
| `cert-manager`                   |                          | TLS cert management in k8s           |
| `percona-server-mysql-operator`  | `k8s-ps`, `k8s-mysql`    | MySQL operator                       |
| `percona-xtradb-cluster-operator`| `k8s-pxc`                | PXC operator                         |
| `percona-server-mongodb-operator`| `k8s-psmdb`, `k8s-mongo` | PSMDB operator                       |
| `percona-postgresql-operator`    | `k8s-pg`                 | PostgreSQL operator                  |
| `cloudnative-pg-operator`        | `k8s-cnpg`, `cnpg`       | CloudNativePG operator               |
| `crunchy-postgres-operator`      | `k8s-crunchy`, `crunchy` | Crunchy Postgres for Kubernetes (PGO) |
| `k8s-pmm`                        |                          | PMM inside k8s                       |
| `k8s-pmm-ha`                     | `pmm-ha`                 | PMM HA (Tech Preview) on k8s         |

## Benchmark

| Keyword     | What it is                     |
|-------------|--------------------------------|
| `sysbench`  | Database benchmark driver      |

## Special keywords (not products)

These shape the deploy command without being installed on a node:

| Keyword            | What it does |
|--------------------|--------------|
| `os:<name>`        | Set the base OS for the next node block. Values: `el7`, `el8`, `el9`, `el10`, `jammy`, `focal`, `noble`, `bookworm`, `sles15`, etc. |

**SUSE (`os:sles15`, aliases `sles` / `suse` / `suse15`) is bare-OS only.** It gives a real SLES 15 SP7 node (SLE BCI base image, no subscription needed) with systemd, sshd and `zypper`, but **no product can be installed on it** — nobody publishes zypper repos anydbver can consume, so the version DB has no `sles15` rows. `anydbver deploy node0 os:sles15 pg:17` fails fast with an explanatory message. Offer it only for reproducing SLE-specific behaviour by hand (`anydbver exec node0 -- zypper -n install ...`); for anything needing a database, steer to an EL or Debian OS.
| `install <product>`| Run only the install steps; don't start/configure. Used with `cache:` to bake a reusable image. |
| `cache:<name>`     | Bake (first deploy) or reuse (subsequent) a pre-built container image. |
| `default`          | Alias for `node0` in `master=default`, `ldap-master=default`, `master:default`. |

## Top-level commands

```sh
anydbver deploy ...                # bring up nodes
anydbver list                      # show running containers in the current namespace
anydbver exec <node> [-- <cmd>]    # run a command inside a node
anydbver destroy [--remove-cache]  # tear down current namespace (and optionally cache images)
anydbver namespace list            # list all namespaces with running containers
anydbver update                    # refresh the local version DB from GitHub master
anydbver shell                     # enter a container with kubectl + helm wired up
anydbver test list                 # list every built-in test recipe
anydbver deploy help keywords      # all keywords (canonical)
anydbver deploy help <keyword>     # subcommands + verbatim CLI examples
anydbver --version                 # binary version
```

## Default versions (snapshot, will rot)

These were the latest defaults on 2026-08-21. They shift between releases as the version DB updates — **always pin** when reproducing a bug.

| Product | `latest` resolves to (approx.) |
|---------|-------------------------------|
| `ps`            | 8.4.x         |
| `mysql`         | 26.7.x        |
| `mariadb`       | 12.3.x        |
| `pxc`           | 8.4.x         |
| `pg`            | 18.x          |
| `ppg`           | 18.x          |
| `psmdb`         | 8.3.x         |
| `pmm`           | 3.x           |
| `pmm-client`    | `3.x.y-1` (bare `3.x.y` won't resolve) |

**v0.1.39 moved three of these onto a newer series.** `psmdb:latest` is 8.3 now,
not 8.0; `mysql:latest` is 26.7 (MySQL switched to calendar versioning after
9.7); `mariadb:latest` is 12.3, not 11.8. If someone wants the previous LTS-ish
line, pin it: `psmdb:8.0`, `mysql:8.4`, `mariadb:11.8`.

If a deploy command says `:latest` and the user is reproducing a bug, **replace it with an explicit patch version** and confirm with `anydbver deploy help <keyword>` which patch your build's version DB has.

## Architecture (Apple Silicon)

Containers share the host kernel, so an arm64 Mac can only install aarch64
packages, and the version DB carries aarch64 rows for a subset of what x86_64
has. `ps`, `mysql`, `mariadb`, `psmdb`, `pbm`, `pg`, `ppg` and `pmm-client` all
have aarch64 builds. **`pxc` and `pxb` (Percona XtraBackup) have none**, so any
PXC topology or xtrabackup step is x86_64 only.

Check before promising a version:

```
anydbver versions ps --arch aarch64
```

Since v0.1.42 a version that exists only for x86_64 is refused up front, naming
the arch and listing what this machine can install. Older builds died minutes
later inside the ansible role with `'dict object' has no attribute ''`.
