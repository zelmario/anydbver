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
   - [PostgreSQL replication](#postgresql-replication)
   - [PostgreSQL HA with Patroni / repmgr / HAProxy](#postgresql-ha-with-patroni--repmgr--haproxy)
   - [MongoDB / PSMDB replica set](#mongodb--psmdb-replica-set)
   - [MongoDB sharded cluster](#mongodb-sharded-cluster)
   - [Backups (pgbackrest, PBM, MinIO)](#backups-pgbackrest-pbm-minio)
   - [PMM (monitoring)](#pmm-monitoring)
   - [Kubernetes operators (k3d)](#kubernetes-operators-k3d)
   - [LDAP / Kerberos authentication](#ldap--kerberos-authentication)
   - [Benchmarking with sysbench](#benchmarking-with-sysbench)
9. [Docker-image mode](#docker-image-mode)
10. [Managing the environment](#managing-the-environment)
11. [Getting help from the CLI](#getting-help-from-the-cli)
12. [Troubleshooting](#troubleshooting)

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
anydbver deploy ps:8.0  node1  ps:8.0,master=node0
```

Reading left to right:
1. On node0: install `ps` (Percona Server), version `8.0`.
2. Switch to `node1`.
3. On node1: install `ps` version `8.0`, with option `master=node0`
   (i.e. be a replica of node0).

---

## Cheatsheet

```sh
# Single node, latest Percona Server
anydbver deploy ps

# Single node, specific PostgreSQL version
anydbver deploy pg:16

# Two nodes — MySQL source + replica
anydbver deploy ps:8.0  node1  ps:8.0,master=node0

# Three-node PXC Galera cluster
anydbver deploy pxc  node1 pxc:latest,master=node0,galera  node2 pxc:latest,master=node0,galera

# Three-node PSMDB replica set
anydbver deploy psmdb:latest,replica-set=rs0 \
  node1 psmdb:latest,replica-set=rs0,master=node0 \
  node2 psmdb:latest,replica-set=rs0,master=node0

# PostgreSQL + Patroni + pgbackrest (two nodes)
anydbver deploy ppg:16 patroni:cluster=cluster1 pgbackrest \
  node1 ppg:16,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest

# PMM server + monitored MySQL node
anydbver deploy pmm:2.42.0,docker-image,port=12443 \
  node1 ps:latest pmm-client:2.42.0-6,server=node0

# PXC operator on k3d
anydbver deploy k3d k8s-pxc:1.13.0

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

| Keyword                   | Aliases               | What it is                                |
|---------------------------|-----------------------|-------------------------------------------|
| `percona-server`          | `ps`                  | Percona Server for MySQL                  |
| `percona-xtradb-cluster`  | `pxc`                 | Percona XtraDB Cluster (Galera)           |
| `mysql`                   |                       | Oracle MySQL Community                    |
| `mariadb`                 |                       | MariaDB                                   |
| `postgresql`              | `pg`                  | PostgreSQL from PGDG                      |
| `percona-postgresql`      | `ppg`                 | Percona Distribution for PostgreSQL       |
| `percona-server-mongodb`  | `psmdb`               | Percona Server for MongoDB                |
| `valkey`                  |                       | Valkey (Redis fork)                       |

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

### Monitoring / Observability

| Keyword       | What it is                             |
|---------------|----------------------------------------|
| `pmm-server` / `pmm` | Percona Monitoring and Management server |
| `pmm-client`  | Percona Monitoring and Management agent |

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
anydbver deploy ps           # latest supported major
anydbver deploy ps:latest    # same as above — explicit
anydbver deploy ps:8.0       # latest 8.0.x
anydbver deploy ps:8.0.29    # exact version
```

- The mapping from short version → concrete package version lives in
  `anydbver_version.sql` (compiled into `~/.config/anydbver/anydbver_version.db`).
- Run `anydbver update` to refresh the local version DB from GitHub master.
- For PMM client specifically, the version also carries a release suffix:
  `pmm-client:2.42.0-6`.

When you want an unmodified upstream container instead of package-based
install, use [docker-image mode](#docker-image-mode).

---

## Choosing the OS

Add `os:<name>` as a node item to change the base OS for that node:

```sh
anydbver deploy os:el8 ps         # Percona Server 8.x on Rocky Linux 8
anydbver deploy os:el9 pg:16      # PostgreSQL 16 on Rocky Linux 9
anydbver deploy os:jammy kerberos # Kerberos KDC on Ubuntu Jammy
anydbver deploy os:el7 ppg:13.5   # Percona PG 13.5 on CentOS 7
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
| `master=nodeN` / `primary=nodeN` | Streaming replica of `nodeN`           |
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

### sysbench

| Option                | Meaning                                             |
|-----------------------|-----------------------------------------------------|
| `mysql=nodeN`         | Target MySQL node                                   |
| `port=6446`           | Target port (e.g. MySQL Router RW port)             |
| `oltprw`              | Run the OLTP read/write workload                    |

---

## Examples by scenario

All commands below are complete — you can copy them verbatim. Before
running another one, tear down the previous environment with
`anydbver destroy` (or pass `--keep` to add to the existing one).

### MySQL / Percona Server / MariaDB

```sh
# Standalone
anydbver deploy ps:5.7
anydbver deploy ps:8.0
anydbver deploy mysql:5.7.35
anydbver deploy mariadb:latest

# Async replication (source + replica)
anydbver deploy ps:5.7.35  node1 ps:5.7.35,master=node0

# Async replication without GTID
anydbver deploy ps:8.0,nogtid  node1 ps:8.0,nogtid,master=node0

# Expose MySQL on the host (port 3306)
anydbver deploy ps:latest,expose=3306
```

### Percona XtraDB Cluster (Galera)

```sh
# 3-node PXC
anydbver deploy pxc \
  node1 pxc:latest,master=node0,galera \
  node2 pxc:latest,master=node0,galera

# 3-node MariaDB Galera
anydbver deploy mariadb:latest,galera \
  node1 mariadb:latest,master=node0,galera \
  node2 mariadb:latest,master=node0,galera
```

### MySQL Group Replication + Router

```sh
anydbver deploy \
  node0 ps:8.0,group-replication \
  node1 ps:8.0,group-replication,master=node0 \
  node2 ps:8.0,mysql-router,master=node0
```

### PostgreSQL replication

```sh
# Streaming replication
anydbver deploy node0 pg:latest,wal=logical \
  node1 pg:latest,primary=node0,wal=logical

# PostgreSQL + repmgr (3 nodes)
anydbver deploy pg:16 repmgr \
  node1 pg:16,master=node0 repmgr \
  node2 pg:16,master=node0 repmgr
```

### PostgreSQL HA with Patroni / repmgr / HAProxy

```sh
# Percona PG + Patroni + pgbackrest, 2 nodes
anydbver deploy \
  ppg:16 patroni:cluster=cluster1 pgbackrest \
  node1 ppg:16,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest

# Full stack: Patroni (3) + HAProxy + pgbouncer
anydbver deploy \
  ppg:16 patroni:cluster=cluster1 pgbackrest \
  node1 ppg:16,master=node0 patroni:master=node0,cluster=cluster1 \
  node2 ppg:16,master=node0 patroni:master=node0,cluster=cluster1 \
  node3 haproxy-patroni:node0,node1,node2 \
  node4 pgbouncer:node3
```

### MongoDB / PSMDB replica set

```sh
# Standalone
anydbver deploy psmdb:latest

# 3-node replica set
anydbver deploy \
  psmdb:5.0,replica-set=rs0 \
  node1 psmdb:5.0,replica-set=rs0,master=node0 \
  node2 psmdb:5.0,replica-set=rs0,master=node0
```

### MongoDB sharded cluster

A full sharded cluster needs shards (each a replica set), config servers
(replica set), and one or more `mongos` routers. See `MONGO.md` for a
larger 3-shard example. A compact 2-shard layout:

```sh
anydbver deploy \
  node0 psmdb:latest,replica-set=rs0,role=shard \
  node1 psmdb:latest,replica-set=rs0,role=shard,master=node0 \
  node2 psmdb:latest,replica-set=rs0,role=shard,master=node0 \
  node3 psmdb:latest,replica-set=rs1,role=shard \
  node4 psmdb:latest,replica-set=rs1,role=shard,master=node3 \
  node5 psmdb:latest,replica-set=rs1,role=shard,master=node3 \
  node6 psmdb:latest,replica-set=cfg0,role=cfg \
  node7 psmdb:latest,replica-set=cfg0,role=cfg,master=node6 \
  node8 psmdb:latest,replica-set=cfg0,role=cfg,master=node6 \
  node9 psmdb:latest \
        mongos-cfg:cfg0/node6,node7,node8 \
        mongos-shard:rs0/node0,node1,node2,rs1/node3,node4,node5
```

### Backups (pgbackrest, PBM, MinIO)

```sh
# PostgreSQL backed up to MinIO via pgbackrest
anydbver deploy minio:docker-image  node1 pg pgbackrest:s3=node0

# PSMDB replica set backed up via PBM to MinIO
anydbver deploy \
  minio:docker-image \
  node1 psmdb:latest,replica-set=rs0                    pbm:latest,s3=node0/backup \
  node2 psmdb:latest,replica-set=rs0,master=node1       pbm:latest,s3=node0/backup
```

### PMM (monitoring)

```sh
# PMM 2.x server + PS nodes with PMM client
anydbver deploy pmm:2.42.0,docker-image,port=12443 \
  node1 ps:latest,group-replication pmm-client:2.42.0-6,server=node0 \
  node2 ps:latest,group-replication,master=node1 pmm-client:2.42.0-6,server=node0 \
  node3 ps:latest,group-replication,master=node1 pmm-client:2.42.0-6,server=node0

# PMM using dev/nightly images
anydbver deploy \
  pmm:docker-image=perconalab/pmm-server:dev-latest,port=12443 \
  node1 mysql:latest,docker-image \
  node2 pmm-client:docker-image=perconalab/pmm-client:dev-latest,server=node0,mysql=node1
```

PMM 3.x uses ports 8443 / 8080 inside the container; pick the host port
with the `port=` option as shown above. PMM 2.x uses 443.

### Kubernetes operators (k3d)

```sh
# Percona XtraDB Cluster operator
anydbver deploy k3d k8s-pxc:1.13.0

# Percona PostgreSQL operator
anydbver deploy k3d cert-manager:1.7.2 k8s-pg:2.3.1

# PSMDB operator with custom cluster domain
anydbver deploy \
  k3d:v1.25.16-k3s4,cluster-domain=percona.local \
  cert-manager:1.14.2 \
  k8s-psmdb:1.16.2,replicas=1,shards=0,namespace=db1

# Multiple PSMDB namespaces behind one ingress
anydbver deploy \
  k3d:latest,ingress=443,ingress-type=nginxinc,nodes=3,host-alias="172.17.0.1:r1.percona.local|r2.percona.local|r3.percona.local" \
  cert-manager \
  k8s-psmdb:1.16.2,replicas=1,shards=0,namespace=db1 \
  k8s-psmdb:1.16.2,replicas=1,shards=0,namespace=db2 \
  k8s-psmdb:1.16.2,replicas=1,shards=0,namespace=db3
```

To drive kubectl/helm yourself against the created cluster:

```sh
anydbver shell    # enters a container with kubectl + helm wired up
```

### LDAP / Kerberos authentication

```sh
# PSMDB 5.0 with LDAP auth
anydbver deploy ldap  node1 ldap-master:default psmdb:5.0

# Kerberos KDC on node0, PSMDB with GSSAPI on node1
anydbver deploy \
  node0 os:jammy kerberos \
  node1 os:el8 psmdb:latest,kerberos-server=node0
```

### Benchmarking with sysbench

```sh
# Simple: sysbench against a single PS node
anydbver deploy ps:5.7  node1 sysbench:latest,mysql=node0

# OLTP R/W against MySQL Router (Group Replication, port 6446)
anydbver deploy \
  node0 ps:8.0,group-replication \
  node1 ps:8.0,group-replication,master=node0 \
  node2 ps:8.0,mysql-router,master=node0 \
  node3 ps:8.0 sysbench:latest,mysql=node2,port=6446,oltprw
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

Without `--keep`, a new `deploy` tears down the previous one first.
With `--keep`, the new node is added:

```sh
anydbver deploy node0 ps
anydbver deploy --keep node1 ps:master=node0
```

### Namespaces (parallel environments)

Give an environment a name so it can coexist with another:

```sh
anydbver --namespace=ns1 deploy ps:8.0
anydbver --namespace=ns2 deploy pg:16
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
