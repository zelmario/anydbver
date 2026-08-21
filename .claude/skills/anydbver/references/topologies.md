# Topologies — recipes per family

> Verified on 2026-08-21. Each section: deploy command → expected node layout → host-side artifacts → verification → "use this when". Versions will drift; pin for bug repro.

## Contents

- [Single node](#single-node)
- [Async / streaming replication](#replication)
- [Multi-node clusters (PXC, MariaDB Galera, MySQL GR, Patroni HA, PSMDB RS, PSMDB sharded, repmgr)](#clusters)
- [Backups](#backups)
- [Operators on K3D](#operators)

<a id="single-node"></a>
## Single node

```sh
anydbver deploy ps                              # latest PS
anydbver deploy pg:18                           # PG 18
anydbver deploy psmdb:8.0                       # PSMDB 8.0
anydbver deploy mariadb:11.4                    # MariaDB 11.4 LTS
anydbver deploy valkey:unstable,docker-image    # Valkey from upstream image
```

**Layout.** One container `<namespace>-<user>-node0`, holding the DB and ssh.

**Verify.**
```sh
echo 'select version()' | anydbver exec node0 -- mysql
echo '\du'              | anydbver exec node0 -- psql -U postgres
echo 'rs.status()'      | anydbver exec node0 -- mongosh
```

**Teardown.** `anydbver destroy`.

**Use this when** — quickest possible test of a version, packaging change, or "does it start at all".

<a id="replication"></a>
## Async / streaming replication

```sh
# MySQL / PS — async with GTIDs
anydbver deploy ps:8.4  node1 ps:8.4,master=node0

# MySQL / PS — async without GTIDs
anydbver deploy ps:8.4,nogtid  node1 ps:8.4,nogtid,master=node0

# PostgreSQL streaming replication
anydbver deploy node0 pg:18,wal=logical \
  node1 pg:18,primary=node0,wal=logical

# 3-node PSMDB replica set
anydbver deploy psmdb:8.0,replica-set=rs0 \
  node1 psmdb:8.0,replica-set=rs0,master=node0 \
  node2 psmdb:8.0,replica-set=rs0,master=node0
```

**Layout.** Two or three containers; node0 is the source/primary, others are replicas.

**Verify.**
```sh
# MySQL
echo 'show replica status\G' | anydbver exec node1 -- mysql
# PG
echo 'select * from pg_stat_wal_receiver' | anydbver exec node1 -- psql -U postgres
# PSMDB
echo 'rs.status().members.map(m=>({name:m.name,state:m.stateStr}))' | anydbver exec node0 -- mongosh
```

**Use this when** — testing failover, WAL/GTID behavior, replica lag, replication-related configuration.

<a id="clusters"></a>
## Multi-node clusters

### PXC / Galera (synchronous)

```sh
anydbver deploy pxc:8.4 \
  node1 pxc:8.4,master=node0,galera \
  node2 pxc:8.4,master=node0,galera
```

**Layout.** 3 PXC nodes; first has no `master=`, that's the bootstrap.

**Verify.**
```sh
echo "show status like 'wsrep_cluster_size'"      | anydbver exec node0 -- mysql
echo "show status like 'wsrep_local_state_comment'" | anydbver exec node1 -- mysql
```

**Use this when** — testing Galera-specific behavior (SST, cluster joins, conflict resolution).

### MariaDB Galera

```sh
anydbver deploy mariadb:11.4,galera \
  node1 mariadb:11.4,master=node0,galera \
  node2 mariadb:11.4,master=node0,galera
```

Same shape as PXC; first node has the `galera` flag and no `master=`.

### MySQL Group Replication + Router

```sh
anydbver deploy \
  node0 ps:8.4,group-replication \
  node1 ps:8.4,group-replication,master=node0 \
  node2 ps:8.4,mysql-router,master=node0
```

**Layout.** node0/node1: GR members; node2: MySQL Router (proxies on port 6446 RW / 6447 RO).

**Verify.**
```sh
echo 'select * from performance_schema.replication_group_members' | anydbver exec node0 -- mysql
mysql -h<router-ip> -P6446 -u root -e 'select @@hostname'
```

**Use this when** — testing InnoDB Cluster, MySQL Router routing, asynchronous-secondary behavior.

### Patroni HA

```sh
# Basic 3-node Patroni on vanilla PG
anydbver deploy pg patroni \
  node1 pg:master=node0 patroni:master=node0 \
  node2 pg:master=node0 patroni:master=node0

# Full stack: Percona PG 17 + Patroni + pgbackrest + HAProxy + pgbouncer
anydbver deploy \
  ppg:17 patroni:cluster=cluster1 pgbackrest \
  node1 ppg:17,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest \
  node2 ppg:17,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest \
  node3 haproxy-patroni:node0,node1,node2 \
  node4 pgbouncer:node3
```

**Layout.** node0..2 = Patroni members (one promotes to leader); node3 = HAProxy-Patroni; node4 = pgbouncer.

**Verify.**
```sh
echo 'patronictl list' | anydbver exec node0
curl http://<node3-ip>:8008/                       # haproxy-patroni stats
psql -h <node4-ip> -p 6432 -U postgres -c '\du'    # via pgbouncer
```

**Use this when** — testing failover, switchover, watchdog behavior, HAProxy-Patroni health-checks.

### Patroni primary + standby clusters

```sh
anydbver deploy \
  ppg:17 patroni:cluster=cluster11 \
  node1 ppg:17,master=node0 patroni:master=node0,cluster=cluster11 \
  node2 ppg:17,master=node0 patroni:master=node0,cluster=cluster11 \
  node3 ppg:17 patroni:standby=node0,cluster=cluster12 \
  node4 ppg:17,master=node3 patroni:master=node3,cluster=cluster12
```

Two Patroni clusters; cluster12 is a standby of cluster11 (continuous WAL stream from node0).

### repmgr

```sh
anydbver deploy pg:18 repmgr \
  node1 pg:18,master=node0 repmgr \
  node2 pg:18,master=node0 repmgr
```

`repmgr` is a separate keyword on each node, not an option on `pg:`.

### PSMDB sharded cluster

See [`examples.md#mongodb-sharded`](examples.md#mongodb-sharded) for the keyfile choreography. **Run the keyfile copy first** or `mongos` will refuse shards.

**Layout.** 9+ nodes — typically 2 shards × 3 + 3 cfg + 1 mongos = 10. With `MONGO.md` style: 3 shards × 3 + 3 cfg + 3 mongos = 15.

**Verify.**
```sh
echo 'sh.status()'           | anydbver exec node9 -- mongosh        # node9 = first mongos
echo 'rs.status().ok'        | anydbver exec node0 -- mongosh        # shard rs0
```

**Use this when** — sharding-specific issues, cross-shard queries, balancer behavior, mongos config.

<a id="backups"></a>
## Backups

### pgbackrest, filesystem repo

```sh
anydbver deploy pg patroni pgbackrest \
  node1 pg:master=node0 patroni:master=node0 pgbackrest \
  node2 pg:master=node0 patroni:master=node0 pgbackrest
```

**Verify.** `anydbver exec node0 -- pgbackrest --stanza=main info`

### pgbackrest, S3 (MinIO)

```sh
anydbver deploy minio:docker-image  node1 pg:18 pgbackrest:s3=node0
```

### pgbackrest, NFS-shared repo across a Patroni cluster

```sh
anydbver deploy nfs \
  node1 pg patroni pgbackrest:repo=/mnt/nfs/pgbackrest nfs-client:server=node0 \
  node2 pg:master=node1 patroni:master=node1 pgbackrest:repo=/mnt/nfs/pgbackrest nfs-client:server=node0 \
  node3 pg:master=node1 patroni:master=node1 pgbackrest:repo=/mnt/nfs/pgbackrest nfs-client:server=node0
```

All three Patroni members share `/mnt/nfs/pgbackrest`. Useful for testing pgbackrest's repo-sharing semantics. **Tear down clients before the NFS server.**

### Barman

```sh
anydbver deploy pg  node1 barman:source=node0
anydbver deploy pg  node1 barman:source=node0,method=streaming-only pg
```

### PBM, MinIO

```sh
anydbver deploy \
  minio:docker-image \
  node1 psmdb:8.0,replica-set=rs0                pbm:latest,s3=node0/backup \
  node2 psmdb:8.0,replica-set=rs0,master=node1   pbm:latest,s3=node0/backup \
  node3 psmdb:8.0,replica-set=rs0,master=node1   pbm:latest,s3=node0/backup
```

**Verify.** `anydbver exec node1 -- pbm status`

### xtrabackup

```sh
anydbver deploy ps:8.0 percona-xtrabackup:8.0
```

PS major must match xtrabackup major.

<a id="operators"></a>
## Operators on K3D

### k3d cluster (alone)

```sh
anydbver deploy k3d
```

**Layout.** Several `k3d-...-server-N` containers running k3s nodes inside.

**Use this when** — you want to drive kubectl yourself; `anydbver shell` enters a container with kubectl/helm pre-wired.

### PXC operator

```sh
anydbver deploy k3d k8s-pxc:1.17.0
anydbver deploy k8s-minio k8s-pxc:1.17.0          # with MinIO for backups
```

**Verify.** `kubectl get pxc; kubectl get pods`

### MySQL operator

```sh
anydbver deploy k3d k8s-ps:0.10.0
```

### Percona PG operator

```sh
anydbver deploy k3d cert-manager:1.14.2 k8s-pg:2.7.0          # cert-manager required
anydbver deploy k8s-pg:2.7.0,db-version=16                    # pin PG major
anydbver deploy k3d k8s-pg:2.7.0 k8s-pg:2.7.0,namespace=pgo1,standby   # primary + standby
```

### CloudNativePG operator

```sh
anydbver deploy k3d k8s-cnpg:1.30.0                                  # 3 instances, default PG
anydbver deploy k3d k8s-cnpg:1.30.0,replicas=1,db-version=17         # single instance, PG 17
anydbver deploy k3d k8s-cnpg:1.30.0,storage=5Gi,memory=1Gi           # size the instances
anydbver deploy k3d k8s-cnpg:1.30.0 k8s-cnpg:1.30.0,cluster-name=db1,namespace=pg1
```

**Verify.** `kubectl get cluster -A; kubectl -n cnpg get pods`

**Differs from the Percona operators:**
- The operator always runs in `cnpg-system` and watches all namespaces — `namespace=`
  places the *Cluster*, not the operator. Extra `k8s-cnpg` keywords reuse the same operator.
- `replicas=N` is the **total** instance count (`replicas=1` → single node).
- `db-version=17` → `ghcr.io/cloudnative-pg/postgresql:17`; a full image reference also works.
- No cert-manager needed. PMM and MinIO backups are **not** wired up for CNPG.
- Superuser access is on: `kubectl -n <ns> exec -it <cluster>-1 -- psql -U postgres`.
  Endpoints `<cluster>-rw` / `-ro` / `-r`; passwords in `<cluster>-app` / `<cluster>-superuser`.

### Crunchy Postgres for Kubernetes (PGO)

```sh
anydbver deploy k3d k8s-crunchy:5.8.8                                # 3 instances, PG 18
anydbver deploy k3d k8s-crunchy:5.8.8,replicas=1,db-version=17       # single instance, PG 17
anydbver deploy k3d k8s-crunchy:5.8.8,storage=5Gi,memory=1Gi         # size the instances
anydbver deploy k3d k8s-crunchy:6.0.2,cluster-name=db1,namespace=pg1

# pgBackRest repo1 on MinIO over HTTPS
anydbver deploy k3d k8s-minio:latest,certs=self-signed cert-manager k8s-crunchy:5.8.8,replicas=1

# LoadBalancer services, backups in a real S3 bucket
anydbver deploy k3d k8s-crunchy:5.8.8,replicas=1,expose,s3=https://KEY:SECRET@s3.eu-west-1.amazonaws.com/my-backups,region=eu-west-1
```

**Verify.** `kubectl get postgrescluster -A; kubectl -n crunchy get pods`

**Differs from the Percona operators:**
- Upstream PGO, the operator Percona's PG operator v2 forked from. Default `5.8.8`,
  any published version deploys (`anydbver versions k8s-crunchy`).
- The operator always runs in `postgres-operator` and watches all namespaces — `namespace=`
  places the *PostgresCluster*. Extra `k8s-crunchy` keywords reuse the same operator.
- `replicas=N` is the **total** instance count, default 3.
- `db-version=17` sets `spec.postgresVersion`, majors 15 to 18 on 5.8 / 6.0.
- The Patroni leader carries `postgres-operator.crunchydata.com/role=master`, not `primary`.
- Images pull anonymously from `registry.developers.crunchydata.com`. New git tags often
  have no image yet, anydbver checks before deploying.
- MinIO backups work: `k8s-minio:latest,certs=self-signed` plus `cert-manager` alongside puts
  pgBackRest `repo1` on the bucket and leaves `repo2` on a volume. The replica-create backup
  fills it immediately. TLS is required, pgBackRest refuses plain HTTP S3. `s3=`, `bucket=`
  and `region=` point it at an external S3 instead. PMM is still **not** wired up for Crunchy.
- `expose` puts `<cluster>-ha` (Patroni's leader service) on a LoadBalancer and gives
  `-replicas` and `-pgbouncer` NodePorts — three LoadBalancers would fight over host port
  5432. `<cluster>-primary` is headless and cannot be exposed.
- `kubectl -n <ns> exec -it <pod> -c database -- psql -U postgres`. Endpoints
  `<cluster>-primary` / `-replicas` / `-pgbouncer`; user in `<cluster>-pguser-<cluster>`.

### PSMDB operator

```sh
# Single PSMDB CR
anydbver deploy \
  k3d:v1.25.16-k3s4,cluster-domain=percona.local \
  cert-manager:1.14.2 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db1

# Multi-namespace behind one ingress
anydbver deploy \
  k3d:latest,ingress=443,ingress-type=nginxinc,nodes=3,host-alias="172.17.0.1:r1.percona.local|r2.percona.local|r3.percona.local" \
  cert-manager \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db1 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db2 \
  k8s-psmdb:1.20.1,replicas=1,shards=0,namespace=db3
```

### Helm install variant

Append `helm` to the operator option:
```sh
anydbver deploy k3d:v1.25.16-k3s4,cluster-domain=percona.local \
  cert-manager:1.14.2 \
  k8s-pg:2.7.0,cluster-name=db1,helm
```

**RAM budget.** Cert-manager + an operator + a 3-replica CR ≈ 6–8 GB. K3D nodes are nested Docker — underprovisioned hosts will see Pending pods and operator CrashLoopBackOff.
