# anydbver — Deployment Instructions

A practical guide to `anydbver deploy`. This document explains the command
structure, every building block of a node definition, and shows a library
of real, copy-pasteable examples for the most common scenarios.

If you only read one section, read [Command Anatomy](#command-anatomy) and
[Cheatsheet](#cheatsheet).

---

## Table of contents

1. [What anydbver does](#what-anydbver-does)
2. [Command Anatomy](#command-anatomy)
3. [Cheatsheet](#cheatsheet)
4. [Products you can deploy](#products-you-can-deploy)
5. [Specifying versions](#specifying-versions)
6. [Choosing the OS](#choosing-the-os)
7. [Node options reference](#node-options-reference)
8. [Examples by scenario](#examples-by-scenario)
   - [MySQL / Percona Server / MariaDB](#mysql--percona-server--mariadb)
   - [Percona XtraDB Cluster (Galera)](#percona-xtradb-cluster-galera)
   - [MySQL Group Replication + Router](#mysql-group-replication--router)
   - [MySQL with Orchestrator / ProxySQL / Xtrabackup](#mysql-with-orchestrator--proxysql--xtrabackup)
   - [PostgreSQL replication](#postgresql-replication)
   - [PostgreSQL HA with Patroni / repmgr / HAProxy](#postgresql-ha-with-patroni--repmgr--haproxy)
   - [PostgreSQL backups with Barman](#postgresql-backups-with-barman)
   - [MongoDB / PSMDB replica set](#mongodb--psmdb-replica-set)
   - [MongoDB sharded cluster](#mongodb-sharded-cluster)
   - [Backups (pgbackrest, PBM, MinIO)](#backups-pgbackrest-pbm-minio)
   - [PMM (monitoring)](#pmm-monitoring)
   - [Kubernetes operators (k3d)](#kubernetes-operators-k3d)
   - [LDAP / Kerberos authentication](#ldap--kerberos-authentication)
   - [Benchmarking with sysbench](#benchmarking-with-sysbench)
9. [Docker-image mode](#docker-image-mode)
10. [Speeding up deploys with `install` + `cache`](#speeding-up-deploys-with-install--cache)
11. [Managing the environment](#managing-the-environment)
12. [Getting help from the CLI](#getting-help-from-the-cli)
13. [Troubleshooting](#troubleshooting)

---

## What anydbver does

`anydbver` spins up multi-node database clusters as Docker containers (or
as nested Docker-in-Docker Kubernetes clusters via k3d). You describe the
topology on a single command line and it runs Ansible inside to install
and configure the software.

It is designed for **short-lived, throwaway** test environments:
reproducing bugs, testing upgrades, trying a new HA topology, etc.

---

## Command Anatomy

Every `anydbver deploy` invocation is a sequence of **node blocks**:

```
anydbver deploy  <node0 block>  node1  <node1 block>  node2  <node2 block>  ...
```

- A **node block** is one or more "things to install on that node",
  separated by spaces.
- Use `nodeN` (literal word, e.g. `node1`, `node2`) to switch the
  following items onto a new node.
- **`node0` is implicit** — whatever comes right after `deploy` applies
  to node0, until the first `nodeN`.

Each "thing to install" has the shape:

```
product:version,option1=value1,option2=value2,flag
```

| Separator | Meaning                               |
|-----------|---------------------------------------|
| `:`       | between the product and its version   |
| `,`       | between the version and options, and between options |
| `=`       | between an option key and its value   |
| space     | moves to the next item on the node, or (when `nodeN` is next) to the next node |

Simplest possible deploy:

```sh
anydbver deploy ps
```

That installs the latest Percona Server for MySQL on node0.

Two nodes with async replication:

```sh
anydbver deploy ps:8.4  node1  ps:8.4,master=node0
```

Reading left to right:
1. On node0: install `ps` (Percona Server), version `8.4`.
2. Switch to `node1`.
3. On node1: install `ps` version `8.4`, with option `master=node0`
   (i.e. be a replica of node0).

---

## Cheatsheet

```sh
# Single node, latest Percona Server
anydbver deploy ps

# Single node, specific PostgreSQL version
anydbver deploy pg:18

# Two nodes — MySQL source + replica
anydbver deploy ps:8.4  node1  ps:8.4,master=node0

# Three-node PXC 8.4 Galera cluster
anydbver deploy pxc:8.4 \
  node1 pxc:8.4,master=node0,galera \
  node2 pxc:8.4,master=node0,galera

# Three-node PSMDB 8.0 replica set
anydbver deploy psmdb:8.0,replica-set=rs0 \
  node1 psmdb:8.0,replica-set=rs0,master=node0 \
  node2 psmdb:8.0,replica-set=rs0,master=node0

# Percona PostgreSQL 17 + Patroni + pgbackrest (two nodes)
anydbver deploy ppg:17 patroni:cluster=cluster1 pgbackrest \
  node1 ppg:17,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest

# PMM 3.7 server + monitored Percona Server node
anydbver deploy pmm:3.7.0,docker-image,port=12443 \
  node1 ps:latest pmm-client:3.7.0-7,server=node0:8443

# PXC operator on k3d
anydbver deploy k3d k8s-pxc:1.17.0

# List running nodes, then destroy everything
anydbver list
anydbver destroy
```

---

## Products you can deploy

You can use the full keyword or any documented **alias**. Ask the CLI for
the authoritative list:

```sh
anydbver deploy help keywords        # all keywords
anydbver deploy help percona-server  # usage + aliases for one keyword
```

### Databases

| Keyword                   | Aliases                      | What it is                                |
|---------------------------|------------------------------|-------------------------------------------|
| `percona-server`          | `ps`, `percona-server-mysql` | Percona Server for MySQL                  |
| `percona-xtradb-cluster`  | `pxc`                        | Percona XtraDB Cluster (Galera)           |
| `mysql`                   |                              | Oracle MySQL Community                    |
| `mariadb`                 |                              | MariaDB                                   |
| `mydb`                    |                              | MySQL-family variant (same options as `mysql`) |
| `postgresql`              | `pg`, `postgres`             | PostgreSQL from PGDG                      |
| `percona-postgresql`      | `ppg`, `percona-postgres`    | Percona Distribution for PostgreSQL       |
| `percona-server-mongodb`  | `psmdb`                      | Percona Server for MongoDB                |
| `valkey`                  |                              | Valkey (Redis fork)                       |

### Replication / HA / proxies

| Keyword                  | What it is                                       |
|--------------------------|--------------------------------------------------|
| `patroni`                | PostgreSQL HA (etcd + auto-failover)             |
| `repmgr`                 | PostgreSQL replication manager                   |
| `percona-orchestrator`   | MySQL topology manager                           |
| `percona-proxysql`       | ProxySQL                                         |
| `haproxy-pg`             | HAProxy tuned for PostgreSQL                     |
| `haproxy-patroni`        | HAProxy tuned for Patroni clusters               |
| `pgbouncer`              | PostgreSQL connection pooler                     |

### Backup

| Keyword                  | What it is                                       |
|--------------------------|--------------------------------------------------|
| `pgbackrest`             | PostgreSQL backup tool                           |
| `barman`                 | PostgreSQL backup manager                        |
| `percona-backup-mongodb` (alias `pbm`) | MongoDB backup tool                |
| `percona-xtrabackup`     | Hot backup tool for MySQL / Percona Server       |

### Monitoring / Observability

| Keyword              | Aliases | What it is                              |
|----------------------|---------|-----------------------------------------|
| `pmm-server`         | `pmm`   | Percona Monitoring and Management server |
| `pmm-client`         |         | Percona Monitoring and Management agent |
| `k8s-pmm`            |         | PMM running inside Kubernetes           |

### Object storage

| Keyword       | What it is                             |
|---------------|----------------------------------------|
| `minio`       | MinIO S3-compatible storage            |
| `k8s-minio`   | MinIO inside Kubernetes                |

### Auth / directory

| Keyword        | What it is                                 |
|----------------|--------------------------------------------|
| `ldap`         | OpenLDAP server                            |
| `ldap-master`  | Point a client node at an LDAP server      |
| `kerberos`     | Kerberos (Samba-based) KDC                 |

### Kubernetes

| Keyword                          | Aliases             | What it is                           |
|----------------------------------|---------------------|--------------------------------------|
| `k3d`                            |                     | Create a nested k3d cluster          |
| `cert-manager`                   |                     | TLS cert management in k8s           |
| `percona-server-mysql-operator`  | `k8s-ps`, `k8s-mysql` | MySQL operator                     |
| `percona-xtradb-cluster-operator`| `k8s-pxc`           | PXC operator                         |
| `percona-server-mongodb-operator`| `k8s-psmdb`, `k8s-mongo` | PSMDB operator                   |
| `percona-postgresql-operator`    | `k8s-pg`            | PostgreSQL operator                  |
| `k8s-pmm`                        |                     | PMM inside k8s                       |

### Benchmark

| Keyword     | What it is                     |
|-------------|--------------------------------|
| `sysbench`  | Database benchmark driver      |

---

## Specifying versions

Four forms, in order of specificity:

```sh
anydbver deploy ps           # latest supported major (currently 8.4)
anydbver deploy ps:latest    # same as above — explicit
anydbver deploy ps:8.4       # latest 8.4.x
anydbver deploy ps:8.4.5     # exact version
anydbver deploy ps:8.0       # latest 8.0.x (previous LTS)
```

- The mapping from short version → concrete package version lives in
  `anydbver_version.sql` (compiled into `~/.config/anydbver/anydbver_version.db`).
- Run `anydbver update` to refresh the local version DB from GitHub master.
- For PMM client specifically, the version also carries a release suffix:
  `pmm-client:3.7.0-7`.

When you want an unmodified upstream container instead of package-based
install, use [docker-image mode](#docker-image-mode).

---

## Choosing the OS

Add `os:<name>` as a node item to change the base OS for that node:

```sh
anydbver deploy os:el8 ps         # Percona Server (latest) on Rocky Linux 8
anydbver deploy os:el9 pg:18      # PostgreSQL 18 on Rocky Linux 9
anydbver deploy os:el10 pg:18     # PostgreSQL 18 on Rocky Linux 10
anydbver deploy os:jammy kerberos # Kerberos KDC on Ubuntu Jammy
anydbver deploy os:el8 ppg:17     # Percona PG 17 on Rocky Linux 8
```

Common OS keywords: `el7`, `el8`, `el9`, `el10` (RHEL/Rocky family),
`jammy`, `focal` (Ubuntu), `bookworm` (Debian). The menu per-node is
driven by `anydbver_version.sql`.

---

## Node options reference

Options are passed as `,key=value` or as bare flags after the version.
The same key often means different things per product — the CLI help is
the authoritative source (`anydbver deploy help <keyword>`). These are
the ones you will use most often.

> The authoritative list of options for every product is printed by
> `anydbver deploy help <keyword>` — consult it when an option below
> isn't enough. The tables in this section only list options that have
> a **documented real example** in the CLI or README.

### Topology (MySQL / Percona Server / MariaDB / PXC)

| Option              | Meaning                                             |
|---------------------|-----------------------------------------------------|
| `master=nodeN`      | Configure as replica of `nodeN`                     |
| `master=default`    | Replica of node0                                    |
| `group-replication` | Join MySQL Group Replication (InnoDB Cluster)       |
| `galera`            | Join Galera cluster (PXC, MariaDB)                  |
| `mysql-router`      | Install MySQL Router on this node                   |
| `nogtid`            | Async replication **without** GTIDs                 |
| `expose=3306`       | Publish the port to the host                        |
| `rocksdb`           | Enable RocksDB storage engine                       |

### Topology (PostgreSQL)

| Option              | Meaning                                             |
|---------------------|-----------------------------------------------------|
| `master=nodeN`      | Streaming replica of `nodeN`                        |
| `primary=nodeN`     | Same as `master=` in PG (either form works)         |
| `wal=logical`       | Enable logical replication slot                     |
| `cluster=name`      | Patroni cluster name                                |
| `standby=nodeN`     | Patroni standby cluster pointing at another cluster |
| `etcd-ip=nodeN`     | Use `nodeN` as etcd endpoint                        |
| `clustercheck`      | Enable HAProxy-compatible health endpoint           |

### Topology (MongoDB / PSMDB)

| Option                | Meaning                                             |
|-----------------------|-----------------------------------------------------|
| `replica-set=rs0`     | Join replica set `rs0`                              |
| `master=nodeN`        | Follow the primary on `nodeN`                       |
| `role=shard` / `shardsrv` | Run this mongod as a shard member               |
| `role=cfg` / `configsrv`  | Run this mongod as a config server              |
| `mongos-cfg:name/n0,n1,n2`            | mongos: config-server RS definition |
| `mongos-shard:rs0/n0,n1,n2,rs1/...`   | mongos: shard RS list               |

### Backup

| Option                | Meaning                                             |
|-----------------------|-----------------------------------------------------|
| `s3=nodeN/path`       | Use MinIO on `nodeN`, bucket path                   |
| `source=nodeN`        | Barman backup source                                |
| `method=rsync` / `method=streaming-only` | Barman backup method             |

### Container / image

| Option                                       | Meaning                              |
|----------------------------------------------|--------------------------------------|
| `docker-image`                               | Use an unmodified upstream image     |
| `docker-image=registry/image:tag`            | Use a specific image and tag         |
| `expose=<port>`                              | Publish port to the host             |
| `admin-port=9091:9090`                       | Explicit host:container port map     |

### Authentication

| Option                     | Meaning                                         |
|----------------------------|-------------------------------------------------|
| `ldap-master=default`      | Bind to the `ldap` server on node0              |
| `kerberos-server=nodeN`    | Use `nodeN` as the KDC (for PSMDB GSSAPI, etc.) |

### Kubernetes (k3d, operators)

| Option                  | Meaning                                            |
|-------------------------|----------------------------------------------------|
| `nodes=3`               | Number of k3d nodes                                |
| `cluster-domain=...`    | DNS domain for the cluster                         |
| `ingress=443`           | Ingress host port                                  |
| `ingress-type=nginxinc` | Ingress controller flavor                          |
| `host-alias="IP:h1\|h2"`| Extra /etc/hosts entries                           |
| `namespace=db1`         | k8s namespace to deploy into                       |
| `replicas=N`            | Operator replica count                             |
| `shards=N`              | Operator shard count                               |
| `cluster-name=db1`      | Logical cluster name created by the operator       |
| `db-version=13`         | DB version the operator should provision           |
| `helm`                  | Install the operator via Helm                      |
| `standby`               | Create a standby cluster (k8s-pg)                  |
| `proxysql`              | Enable ProxySQL in the cluster (k8s-pxc)           |
| `certs=self-signed`     | Use self-signed TLS certificates (k8s-minio)       |

### Valkey

| Option      | Meaning                                                     |
|-------------|-------------------------------------------------------------|
| `master=nodeN` | Replicate from `nodeN`                                   |
| `sentinel`  | Run Valkey Sentinel on this node                            |
| `cluster=name` | Valkey cluster name                                      |

### sysbench

| Option                | Meaning                                             |
|-----------------------|-----------------------------------------------------|
| `mysql=nodeN`         | Target MySQL node                                   |
| `port=6446`           | Target port (e.g. MySQL Router RW port)             |
| `oltprw`              | Run the OLTP read/write workload                    |

---

## Examples by scenario

Every command below is a real, documented example — either verbatim from
`anydbver deploy help <keyword>`, or from `README.md` / `MONGO.md` /
`CACHING.md`. Before running another one, tear down the previous
environment with `anydbver destroy` (or pass `--keep` to add to the
existing one).

### MySQL / Percona Server / MariaDB

```sh
# Standalone
anydbver deploy ps              # latest (Percona Server 8.4)
anydbver deploy ps:8.4          # latest 8.4.x
anydbver deploy ps:8.4.5        # exact version
anydbver deploy mysql:8.4       # Oracle MySQL 8.4 LTS
anydbver deploy mariadb:11.4    # MariaDB 11.4 LTS

# Async replication (source + replica) — Percona Server 8.4
anydbver deploy ps:8.4  node1 ps:8.4,master=node0

# Same pattern for Oracle MySQL, explicit OS
anydbver deploy mysql:8.4 os:el9  node1 mysql:8.4,master=node0 os:el9

# Async replication without GTID
anydbver deploy ps:8.4,nogtid  node1 ps:8.4,nogtid,master=node0

# Expose MySQL on the host (port 3306)
anydbver deploy ps:latest,expose=3306
```

### Percona XtraDB Cluster (Galera)

```sh
# 3-node PXC 8.4
anydbver deploy pxc:8.4 \
  node1 pxc:8.4,master=node0,galera \
  node2 pxc:8.4,master=node0,galera

# 3-node PXC 8.0 — galera members chained via node1
anydbver deploy pxc:8.0 \
  node1 pxc:8.0,master=node0 \
  node2 pxc:8.0,master=node1,galera \
  node3 pxc:8.0,master=node1,galera

# 3-node MariaDB Galera
anydbver deploy mariadb:11.4,galera \
  node1 mariadb:11.4,master=node0,galera \
  node2 mariadb:11.4,master=node0,galera
```

### MySQL Group Replication + Router

```sh
anydbver deploy \
  node0 ps:8.4,group-replication \
  node1 ps:8.4,group-replication,master=node0 \
  node2 ps:8.4,mysql-router,master=node0
```

### MySQL with Orchestrator / ProxySQL / Xtrabackup

```sh
# ProxySQL in front of a source+replica pair
anydbver deploy ps:8.4  node1 ps:8.4,master=node0  node2 percona-proxysql:latest,master=node0

# MySQL Orchestrator watching a 3-node chain
anydbver deploy \
  ps:8.4 \
  node1 ps:8.4,master=node0 \
  node2 ps:8.4,master=node1 \
  node3 percona-orchestrator:latest,master=node0

# Load a SQL dump into a fresh PS 8.0 and install xtrabackup 8.0
# (PS 8.0 + PXB 8.0 are paired — PXB 8.0 backs up PS 8.0)
anydbver deploy \
  ps:8.0,rocksdb,sql=http://user:pass@172.17.0.1:9000/sampledb/world.sql \
  percona-xtrabackup:8.0
```

### PostgreSQL replication

```sh
# Streaming replication (PostgreSQL 18)
anydbver deploy node0 pg:18,wal=logical \
  node1 pg:18,primary=node0,wal=logical

# PostgreSQL 18 + repmgr (3 nodes)
anydbver deploy pg:18 repmgr \
  node1 pg:18,master=node0 repmgr \
  node2 pg:18,master=node0 repmgr
```

### PostgreSQL HA with Patroni / repmgr / HAProxy

```sh
# Basic 3-node Patroni on vanilla PG
anydbver deploy pg patroni \
  node1 pg:master=node0 patroni:master=node0 \
  node2 pg:master=node0 patroni:master=node0

# HAProxy-PG load balancer in front of a clustercheck-enabled chain
anydbver deploy haproxy-pg:node1,node2,node3 \
  node1 pg:clustercheck \
  node2 pg:master=node1,clustercheck \
  node3 pg:master=node1,clustercheck

# pgbouncer in front of a primary + 2 replicas
anydbver deploy pgbouncer:node1,node2,node3 \
  node1 pg \
  node2 pg:master=node1 \
  node3 pg:master=node1

# Full stack: Percona PG 17 + Patroni + pgbackrest (3 nodes) + HAProxy + pgbouncer
anydbver deploy \
  ppg:17 patroni:cluster=cluster1 pgbackrest \
  node1 ppg:17,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest \
  node2 ppg:17,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest \
  node3 haproxy-patroni:node0,node1,node2 \
  node4 pgbouncer:node3

# Primary Patroni cluster + a standby Patroni cluster (Percona PG 17)
anydbver deploy \
  ppg:17 patroni:cluster=cluster11 \
  node1 ppg:17,master=node0 patroni:master=node0,cluster=cluster11 \
  node2 ppg:17,master=node0 patroni:master=node0,cluster=cluster11 \
  node3 ppg:17 patroni:standby=node0,cluster=cluster12 \
  node4 ppg:17,master=node3 patroni:master=node3,cluster=cluster12
```

### PostgreSQL backups with Barman

Barman is a pull-style backup manager for PostgreSQL. It lives on its own
node and reads from the PG source you point it at.

```sh
# Default Barman topology: rsync/pg_basebackup from node0
anydbver deploy pg  node1 barman:source=node0

# Streaming-only (continuous WAL streaming, no rsync)
anydbver deploy pg  node1 barman:source=node0,method=streaming-only pg
```

### MongoDB / PSMDB replica set

```sh
# Single PSMDB + Percona Backup for MongoDB (no replica set)
anydbver deploy node0 psmdb pbm

# 3-node PSMDB 8.0 replica set backed up to a MinIO on node0
anydbver deploy minio:docker-image \
  node1 psmdb:8.0,replica-set=rs0                    pbm:latest,s3=node0/backup \
  node2 psmdb:8.0,replica-set=rs0,master=node1       pbm:latest,s3=node0/backup \
  node3 psmdb:8.0,replica-set=rs0,master=node1       pbm:latest,s3=node0/backup

# PSMDB 8.0 authenticating against an LDAP server on node0
anydbver deploy ldap node1 ldap-master:default psmdb:8.0
```

### MongoDB sharded cluster

A full sharded cluster needs shards (each a replica set), config servers
(replica set), and one or more `mongos` routers. Same shape as the
`anydbver deploy help percona-server-mongodb` 2-shard example, with the
version bumped to PSMDB 8.0:

```sh
anydbver deploy \
  psmdb:8.0,replica-set=rs0,role=shard \
  node1 psmdb:8.0,replica-set=rs0,role=shard,master=node0 \
  node2 psmdb:8.0,replica-set=rs0,role=shard,master=node0 \
  node3 psmdb:8.0,replica-set=rs1,role=shard \
  node4 psmdb:8.0,replica-set=rs1,role=shard,master=node3 \
  node5 psmdb:8.0,replica-set=rs1,role=shard,master=node3 \
  node6 psmdb:8.0,replica-set=cfg0,role=cfg \
  node7 psmdb:8.0,replica-set=cfg0,role=cfg,master=node6 \
  node8 psmdb:8.0,replica-set=cfg0,role=cfg,master=node6 \
  node9 psmdb:8.0 \
        mongos-cfg:cfg0/node6,node7,node8 \
        mongos-shard:rs0/node0,node1,node2,rs1/node3,node4,node5
```

The same layout with `psmdb:latest` and PBM on every node is also a
documented example — see the `pbm`-augmented 9-node command in
`anydbver deploy help percona-server-mongodb`. See `MONGO.md` for a
3-shard variant.

### Backups (pgbackrest, PBM, MinIO)

```sh
# PostgreSQL 18 backed up to MinIO via pgbackrest
anydbver deploy minio:docker-image  node1 pg:18 pgbackrest:s3=node0

# PSMDB 8.0 replica set backed up via PBM to MinIO
anydbver deploy \
  minio:docker-image \
  node1 psmdb:8.0,replica-set=rs0                    pbm:latest,s3=node0/backup \
  node2 psmdb:8.0,replica-set=rs0,master=node1       pbm:latest,s3=node0/backup
```

### PMM (monitoring)

```sh
# PMM 3.7 server + 3-node group-replication cluster with PMM client
anydbver deploy pmm:3.7.0,docker-image,port=12443 \
  node1 ps:latest,group-replication pmm-client:3.7.0-7,server=node0:8443 \
  node2 ps:latest,group-replication,master=node1 pmm-client:3.7.0-7,server=node0:8443 \
  node3 ps:latest,group-replication,master=node1 pmm-client:3.7.0-7,server=node0:8443

# Minimal PMM 3.7: server + one monitored node
anydbver deploy pmm:3.7.0,docker-image,port=12443 \
  node1 ps:latest pmm-client:3.7.0-7,server=node0:8443

# PMM 2.x is still supported for legacy deployments (port 443 inside)
anydbver deploy pmm:2.42.0,docker-image,port=12443 \
  node1 ps:latest pmm-client:2.42.0-6,server=node0

# PMM using dev/nightly images
anydbver deploy \
  pmm:docker-image=perconalab/pmm-server:dev-latest,port=12443 \
  node1 mysql:latest,docker-image \
  node2 pmm-client:docker-image=perconalab/pmm-client:dev-latest,server=node0,mysql=node1
```

- PMM 2.x listens on port **443** inside the container — `server=node0`
  is enough.
- PMM 3.x listens on **8443** (HTTPS) / **8080** (HTTP) inside the
  container — the client needs `server=node0:8443`.
- In both cases, pick the **host** port for the UI with `port=12443`
  (or any free port).

### Kubernetes operators (k3d)

```sh
# Percona XtraDB Cluster operator 1.17
anydbver deploy k3d k8s-pxc:1.17.0

# PXC operator on a cluster that also has MinIO for backups
anydbver deploy k8s-minio k8s-pxc:1.17.0

# MySQL operator 0.10
anydbver deploy k3d k8s-ps:0.10.0

# Percona PostgreSQL operator 2.7
anydbver deploy k3d cert-manager:1.14.2 k8s-pg:2.7.0

# PG operator with a primary cluster + standby cluster in another namespace
anydbver deploy k3d k8s-minio:latest,certs=self-signed cert-manager \
  k8s-pg:2.7.0 \
  k8s-pg:2.7.0,namespace=pgo1,standby

# PG operator installed via Helm
anydbver deploy k3d:v1.25.16-k3s4,cluster-domain=percona.local \
  cert-manager:1.14.2 \
  k8s-pg:2.7.0,cluster-name=db1,helm

# PG operator pinned to DB major version 16
anydbver deploy k8s-pg:2.7.0,db-version=16

# PSMDB operator 1.20 with custom cluster domain
anydbver deploy \
  k3d:v1.25.16-k3s4,cluster-domain=percona.local \
  cert-manager:1.14.2 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db1

# Multiple PSMDB namespaces behind one ingress
anydbver deploy \
  k3d:latest,ingress=443,ingress-type=nginxinc,nodes=3,host-alias="172.17.0.1:r1.percona.local|r2.percona.local|r3.percona.local" \
  cert-manager \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db1 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db2 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db3
```

To drive kubectl/helm yourself against the created cluster:

```sh
anydbver shell    # enters a container with kubectl + helm wired up
```

### LDAP / Kerberos authentication

```sh
# PSMDB 8.0 with LDAP auth
anydbver deploy ldap  node1 ldap-master:default psmdb:8.0

# Kerberos KDC on node0, PSMDB with GSSAPI on node1
anydbver deploy \
  node0 os:jammy kerberos \
  node1 os:el8 psmdb:latest,kerberos-server=node0
```

### Benchmarking with sysbench

```sh
# Simple: sysbench against a single Percona Server 8.4 node
anydbver deploy ps:8.4  node1 sysbench:latest,mysql=node0

# OLTP R/W against MySQL Router (Group Replication, port 6446)
anydbver deploy \
  node0 ps:8.4,group-replication \
  node1 ps:8.4,group-replication,master=node0 \
  node2 ps:8.4,mysql-router,master=node0 \
  node3 ps:8.4 sysbench:latest,mysql=node2,port=6446,oltprw
```

---

## Docker-image mode

By default anydbver installs from OS packages, the same way you would on
a real machine. When you want unmodified upstream images (fast restart,
specific tag, dev builds) add `docker-image`:

```sh
# Bare flag: use the `version` field as the image tag
anydbver deploy valkey:unstable,docker-image \
  node1 valkey:unstable,docker-image,master=node0 \
  node2 valkey:unstable,docker-image,master=node0

# Explicit image name and tag
anydbver deploy pmm:docker-image=perconalab/pmm-server:dev-latest,port=12443 \
  node1 mysql:latest,docker-image \
  node2 pmm-client:docker-image=perconalab/pmm-client:dev-latest,server=node0,mysql=node1
```

Supported with `docker-image`: MySQL, PostgreSQL, MongoDB, Valkey, MinIO,
PMM, PMM client.

---

## Speeding up deploys with `install` + `cache`

For repeated deployments of the same software stack you can pre-bake a
container image and reuse it. Two keywords drive this:

- `install <product>` — run the install steps only, do **not** start/
  configure the service.
- `cache:<name>` — the name of the baked image to create (on the first
  deploy) or reuse (on subsequent deploys).

First build the cache, then use it:

```sh
# 1) build a cache image called "ps-8.4.5"
anydbver deploy install ps:8.4.5 cache:ps-8.4.5

# 2) reuse it across all nodes of a real deployment
anydbver deploy \
          install ps:8.4.5 cache:ps-8.4.5 \
  node1   install ps:8.4.5 cache:ps-8.4.5 \
  node2   install ps:8.4.5 cache:ps-8.4.5 \
  default ps:8.4.5 \
  node1   ps:8.4.5 master:default \
  node2   ps:8.4.5 master:default
```

Supported with `install`: MySQL, Percona Server, PSMDB, MariaDB,
PostgreSQL, Patroni, PMM, Samba, k3s. See `CACHING.md` for the full
story (including an optional local nginx proxy cache for package
downloads).

---

## Managing the environment

```sh
anydbver list                           # show running containers
anydbver exec node0                     # /bin/sh inside node0
anydbver exec node0 -- bash -il         # login bash if available
echo 'show variables' | anydbver exec node1 -- mysql

anydbver destroy                        # tear down everything
anydbver destroy --remove-cache         # and wipe the cache images too
```

### Adding to an existing deployment

Without `--keep`, a new `deploy` tears down the previous one first
(this is intentional — anydbver treats a fresh invocation as a fresh
topology).

With `--keep`, the new node is added on top of the existing one:

```sh
anydbver deploy node0 ps
anydbver deploy --keep node1 ps:master=node0
# node1 is now a replica of node0; node0 was left running
```

### Namespaces (parallel environments)

Give an environment a name so it can coexist with another:

```sh
anydbver --namespace=ns1 deploy ps:8.0
anydbver --namespace=ns2 deploy pg:18
anydbver namespace list
anydbver --namespace=ns1 destroy
```

### Provider selection

- Default: Docker containers.
- `--provider=kubectl`: deploy against an already-running Kubernetes
  cluster. (Does not manage the cluster itself.)

---

## Getting help from the CLI

The CLI knows all the keywords, aliases, and subcommands it supports.
Use it — it is the ground truth:

```sh
anydbver deploy help                # top-level
anydbver deploy help keywords       # all products
anydbver deploy help percona-server # one product: options + example
anydbver deploy help k8s-psmdb      # resolves the alias, shows examples
anydbver test list                  # every test command you can copy
```

---

## Troubleshooting

- **Nothing runs / "cannot connect to Docker daemon".** Your user is not
  in the `docker` group. Add it and re-login.
- **"Unknown keyword / unknown option".** Run
  `anydbver deploy help <keyword>` — option names vary between
  products. Typos are the most common cause.
- **`anydbver update` keeps failing.** Check network access to GitHub;
  the version DB is fetched from
  `https://github.com/zelmario/anydbver/raw/master/anydbver_version.sql`.
- **Old cached cluster lingers.** `anydbver destroy --remove-cache`
  forces a clean rebuild next time.
- **Port conflicts.** Use `expose=<host-port>` / `port=<host-port>` to
  choose a free host port instead of the default.
- **PMM page opens over HTTP and refuses to log in.** On Docker Desktop
  this is a known UI limitation; hand-edit the URL to `https://...`.

---

## Where to go next

- `README.md` — install, upgrade, binaries layout
- `MONGO.md` — full MongoDB sharded-cluster example
- `CACHING.md` — cache-image system for faster restarts
- `anydbver deploy help keywords` — the authoritative product list
