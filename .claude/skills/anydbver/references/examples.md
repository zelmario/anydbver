# Examples by scenario

> Verified on 2026-06-04. Every command below is a real, documented example — verbatim from `instructions.md`, `MONGO.md`, `CACHING.md`, or `anydbver deploy help <keyword>`. Versions in these examples will drift; pin explicitly when you reproduce a bug.

Tear down between scenarios with `anydbver destroy` (or use `--namespace=<name>` to keep them parallel).

## MySQL / Percona Server / MariaDB

```sh
# Standalone
anydbver deploy ps              # latest (Percona Server 8.4 as of 2026-05-07)
anydbver deploy ps:8.4          # latest 8.4.x
anydbver deploy ps:8.4.5        # exact version — preferred for bug repro
anydbver deploy mysql:8.4       # Oracle MySQL 8.4 LTS
anydbver deploy mariadb:11.4    # MariaDB 11.4 LTS

# Async replication (source + replica)
anydbver deploy ps:8.4  node1 ps:8.4,master=node0

# Same pattern, explicit OS
anydbver deploy mysql:8.4 os:el9  node1 mysql:8.4,master=node0 os:el9

# Async without GTID
anydbver deploy ps:8.4,nogtid  node1 ps:8.4,nogtid,master=node0

# Expose to host
anydbver deploy ps:latest,expose=3306
```

## Percona XtraDB Cluster (Galera)

```sh
# 3-node PXC 8.4 — fan-out from node0
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

**First node has no `master=`.** That bootstraps the cluster.

## MySQL Group Replication + Router

```sh
anydbver deploy \
  node0 ps:8.4,group-replication \
  node1 ps:8.4,group-replication,master=node0 \
  node2 ps:8.4,mysql-router,master=node0
```

## MySQL with Orchestrator / ProxySQL / xtrabackup

```sh
# ProxySQL in front of source+replica
anydbver deploy ps:8.4  node1 ps:8.4,master=node0  node2 percona-proxysql:latest,master=node0

# Orchestrator watching a 3-node chain
anydbver deploy \
  ps:8.4 \
  node1 ps:8.4,master=node0 \
  node2 ps:8.4,master=node1 \
  node3 percona-orchestrator:latest,master=node0

# Load a SQL dump and pair with xtrabackup (PS 8.0 + PXB 8.0 pair together)
anydbver deploy \
  ps:8.0,rocksdb,sql=http://user:pass@172.17.0.1:9000/sampledb/world.sql \
  percona-xtrabackup:8.0
```

## PostgreSQL replication

```sh
# Streaming replication (PG 18)
anydbver deploy node0 pg:18,wal=logical \
  node1 pg:18,primary=node0,wal=logical

# PG 18 + repmgr (3 nodes)
anydbver deploy pg:18 repmgr \
  node1 pg:18,master=node0 repmgr \
  node2 pg:18,master=node0 repmgr
```

## PostgreSQL HA — Patroni / repmgr / HAProxy / pgbouncer

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

# pgbouncer in front of primary + 2 replicas
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

# Primary + standby Patroni clusters (Percona PG 17)
anydbver deploy \
  ppg:17 patroni:cluster=cluster11 \
  node1 ppg:17,master=node0 patroni:master=node0,cluster=cluster11 \
  node2 ppg:17,master=node0 patroni:master=node0,cluster=cluster11 \
  node3 ppg:17 patroni:standby=node0,cluster=cluster12 \
  node4 ppg:17,master=node3 patroni:master=node3,cluster=cluster12
```

## PostgreSQL backups — Barman

```sh
# Default (rsync / pg_basebackup)
anydbver deploy pg  node1 barman:source=node0

# Streaming-only
anydbver deploy pg  node1 barman:source=node0,method=streaming-only pg
```

## MongoDB / PSMDB replica set

```sh
# Single PSMDB + PBM (no replica set)
anydbver deploy node0 psmdb pbm

# 3-node PSMDB 8.0 replica set backed up to MinIO on node0
anydbver deploy minio:docker-image \
  node1 psmdb:8.0,replica-set=rs0                 pbm:latest,s3=node0/backup \
  node2 psmdb:8.0,replica-set=rs0,master=node1    pbm:latest,s3=node0/backup \
  node3 psmdb:8.0,replica-set=rs0,master=node1    pbm:latest,s3=node0/backup

# PSMDB with LDAP auth
anydbver deploy ldap node1 ldap-master:default psmdb:8.0
```

<a id="mongodb-sharded"></a>
## MongoDB sharded cluster

A full sharded cluster needs shards (each a replica set), config servers (replica set), and one or more `mongos` routers. **Before deploying**, share the keyfile across every replica set:

```sh
openssl rand -base64 756 > secret/cfg0-keyfile
for k in rs0-keyfile rs1-keyfile rs2-keyfile; do cp -L secret/cfg0-keyfile secret/$k; done
```

Then the deploy (2 shards, 1 mongos, PSMDB 8.0):

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

For 3 shards see `MONGO.md` (15-node `cache:psmdb-X` form).

## Backups (pgbackrest, PBM, MinIO)

```sh
# PG 18 + pgbackrest writing to MinIO
anydbver deploy minio:docker-image  node1 pg:18 pgbackrest:s3=node0

# PSMDB 8.0 RS backed up via PBM to MinIO
anydbver deploy \
  minio:docker-image \
  node1 psmdb:8.0,replica-set=rs0                 pbm:latest,s3=node0/backup \
  node2 psmdb:8.0,replica-set=rs0,master=node1    pbm:latest,s3=node0/backup
```

<a id="pmm"></a>
## PMM (monitoring)

**Port distinction is the most-missed thing.** PMM 2.x → internal 443 → `server=node0`. PMM 3.x → internal 8443 → `server=node0:8443`. The host port (`port=12443`) is just for opening the UI.

```sh
# PMM 3.7 server + 3-node group-replication cluster with PMM client
anydbver deploy pmm:3.7.0,docker-image,port=12443 \
  node1 ps:latest,group-replication                    pmm-client:3.7.0-7,server=node0:8443 \
  node2 ps:latest,group-replication,master=node1       pmm-client:3.7.0-7,server=node0:8443 \
  node3 ps:latest,group-replication,master=node1       pmm-client:3.7.0-7,server=node0:8443

# Minimal PMM 3.7
anydbver deploy pmm:3.7.0,docker-image,port=12443 \
  node1 ps:latest pmm-client:3.7.0-7,server=node0:8443

# PMM 2.x (port 443 inside)
anydbver deploy pmm:2.42.0,docker-image,port=12443 \
  node1 ps:latest pmm-client:2.42.0-6,server=node0

# PMM with dev/nightly images
anydbver deploy \
  pmm:docker-image=perconalab/pmm-server:dev-latest,port=12443 \
  node1 mysql:latest,docker-image \
  node2 pmm-client:docker-image=perconalab/pmm-client:dev-latest,server=node0,mysql=node1
```

After deploy, open `https://localhost:12443` in your browser. Default login: `admin` / `verysecretpassword1^`.

## Kubernetes operators on K3D

```sh
# PXC operator 1.17
anydbver deploy k3d k8s-pxc:1.17.0

# PXC operator with MinIO for backups
anydbver deploy k8s-minio k8s-pxc:1.17.0

# MySQL operator 0.10
anydbver deploy k3d k8s-ps:0.10.0

# Percona PG operator 2.7 (cert-manager required)
anydbver deploy k3d cert-manager:1.14.2 k8s-pg:2.7.0

# PG operator + standby cluster in another namespace
anydbver deploy k3d k8s-minio:latest,certs=self-signed cert-manager \
  k8s-pg:2.7.0 \
  k8s-pg:2.7.0,namespace=pgo1,standby

# PG operator via Helm
anydbver deploy k3d:v1.25.16-k3s4,cluster-domain=percona.local \
  cert-manager:1.14.2 \
  k8s-pg:2.7.0,cluster-name=db1,helm

# PG operator pinned to PG major 16
anydbver deploy k8s-pg:2.7.0,db-version=16

# PSMDB operator 1.20 with custom cluster domain
anydbver deploy \
  k3d:v1.25.16-k3s4,cluster-domain=percona.local \
  cert-manager:1.14.2 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db1

# Multi-namespace PSMDB behind one ingress
anydbver deploy \
  k3d:latest,ingress=443,ingress-type=nginxinc,nodes=3,host-alias="172.17.0.1:r1.percona.local|r2.percona.local|r3.percona.local" \
  cert-manager \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db1 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db2 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db3
```

After deploy, drive kubectl/helm yourself:
```sh
anydbver shell    # enters a container with kubectl + helm wired up
```

## PMM HA on Kubernetes (Tech Preview)

`k8s-pmm-ha` installs the two-step PMM HA stack (`pmm-ha-dependencies`
operators + `pmm-ha`) and creates the required `pmm-secret`. The
default `size=small` profile shrinks every backend so the full stack
fits one k3d node (~8c/12-15GB); PMM itself stays at 3 replicas so
Raft leader election and HAProxy leader routing are real.

```sh
# Default small profile — single k3d node
anydbver deploy k3d k8s-pmm-ha:1.4.1

# Override the admin password
anydbver deploy k3d k8s-pmm-ha:1.4.1,size=small,password=mysecret

# Real multi-node cluster — chart defaults (requires ≥3 nodes for required anti-affinity)
anydbver deploy k3d:nodes=3 k8s-pmm-ha:1.4.1,size=full

# Custom helm values layered on top of the small profile
anydbver deploy k3d k8s-pmm-ha:1.4.1,values=/path/to/extra.yaml
```

Access the UI through HAProxy (the only supported external entry):

```sh
kubectl port-forward -n pmm svc/pmm-ha-haproxy 8443:443
# then browse https://localhost:8443  (login: admin / <password>)
```

Find the Raft leader and inspect operator-managed backends:

```sh
kubectl get pods -n pmm -l app.kubernetes.io/name=pmm-ha
kubectl get vmcluster,postgrescluster,clickhouseinstallation -n pmm
```

## LDAP / Kerberos authentication

```sh
# PSMDB 8.0 with LDAP auth
anydbver deploy ldap  node1 ldap-master:default psmdb:8.0

# Kerberos KDC on node0, PSMDB with GSSAPI on node1 (cross-OS)
anydbver deploy \
  node0 os:jammy kerberos \
  node1 os:el8 psmdb:latest,kerberos-server=node0
```

## Shared storage with NFS (new in v0.1.33)

```sh
# Smallest working example
anydbver deploy nfs node1 pg nfs-client:server=node0

# Custom mount point
anydbver deploy nfs node1 pg nfs-client:server=node0,mount=/data/shared

# Patroni cluster sharing a pgbackrest repo over NFS
anydbver deploy nfs \
  node1 pg patroni pgbackrest:repo=/mnt/nfs/pgbackrest nfs-client:server=node0 \
  node2 pg:master=node1 patroni:master=node1 pgbackrest:repo=/mnt/nfs/pgbackrest nfs-client:server=node0 \
  node3 pg:master=node1 patroni:master=node1 pgbackrest:repo=/mnt/nfs/pgbackrest nfs-client:server=node0
```

Tear down clients **before** the server. NFS data is tmpfs — ephemeral. See [`options.md#nfs`](options.md) for full gotchas.

## Benchmarking with sysbench

```sh
# Single-node target
anydbver deploy ps:8.4  node1 sysbench:latest,mysql=node0

# OLTP R/W against MySQL Router (Group Replication, port 6446)
anydbver deploy \
  node0 ps:8.4,group-replication \
  node1 ps:8.4,group-replication,master=node0 \
  node2 ps:8.4,mysql-router,master=node0 \
  node3 ps:8.4 sysbench:latest,mysql=node2,port=6446,oltprw
```

<a id="bug-repro"></a>
## Bug reproduction patterns

The canonical use case. Default to `--namespace=<ticket-id>` so the repro is isolated and side-by-side with whatever else is running. **Always pin to an exact patch version** — `:latest` will rot.

```sh
# Reproduce a MySQL 8.4.5 issue with async replication, isolated namespace
anydbver --namespace=PS-9999 deploy ps:8.4.5 node1 ps:8.4.5,master=node0

# Reproduce a PG 18 logical replication issue
anydbver --namespace=PG-1234 deploy node0 pg:18.1,wal=logical \
  node1 pg:18.1,primary=node0,wal=logical

# Reproduce a PSMDB 8.0 sharded-cluster bug — full topology in one isolated namespace
# (remember: keyfile copy first; see "MongoDB sharded cluster" above)
anydbver --namespace=PSMDB-5555 deploy \
  psmdb:8.0.4,replica-set=rs0,role=shard \
  node1 psmdb:8.0.4,replica-set=rs0,role=shard,master=node0 \
  ...

# Tear down after the repro is done
anydbver --namespace=PS-9999 destroy
```

<a id="multi-db"></a>
## Multi-DB / cross-product

One PMM monitoring three different DBs in one namespace:

```sh
anydbver --namespace=multi deploy \
  pmm:3.7.0,docker-image,port=12443 \
  node1 ps:8.4 pmm-client:3.7.0-7,server=node0:8443 \
  node2 pg:18 pmm-client:3.7.0-7,server=node0:8443 \
  node3 psmdb:8.0,replica-set=rs0 pmm-client:3.7.0-7,server=node0:8443
```

Or run different DBs in **separate namespaces** in parallel (when one shouldn't see the other):

```sh
anydbver --namespace=mysql-test deploy ps:8.4 node1 ps:8.4,master=node0
anydbver --namespace=pg-test    deploy pg:18 node1 pg:18,primary=node0
anydbver namespace list                                # see all
anydbver --namespace=mysql-test list                   # only this one
anydbver --namespace=mysql-test destroy                # only tear down this one
```

<a id="environment"></a>
## Environment management

```sh
# Add to existing deploy without destroying
anydbver deploy node0 ps
anydbver deploy --keep node1 ps:master=node0

# Inspect
anydbver list                          # current namespace
anydbver namespace list                # all namespaces
anydbver exec node0                    # /bin/sh
anydbver exec node0 -- bash -il        # login bash
echo 'show variables' | anydbver exec node1 -- mysql

# Tear down
anydbver destroy                       # current namespace only
anydbver destroy --remove-cache        # also wipe cache images
anydbver --namespace=ns1 destroy       # specific namespace

# Refresh version DB (run after upgrading binary or if versions look wrong)
anydbver update
```

To wipe **everything** across all namespaces, iterate `anydbver namespace list`. Confirm with the user first.
