# Options reference (per family)

> Verified on 2026-08-21. **`anydbver deploy help <keyword>` is canonical** — these tables are dated. If you are about to emit a non-trivial command for a keyword combination you have not used in the conversation, run that first.

## MySQL / Percona Server / MariaDB / mydb

| Option              | Meaning                                             |
|---------------------|-----------------------------------------------------|
| `master=nodeN`      | Configure as replica of `nodeN` (alias: `primary=`). |
| `master=default`    | Replica of node0.                                   |
| `group-replication` | Join MySQL Group Replication (InnoDB Cluster).      |
| `galera`            | Join Galera cluster (PXC, MariaDB).                 |
| `mysql-router`      | Install MySQL Router on this node.                  |
| `nogtid`            | Async replication **without** GTIDs.                |
| `expose=3306`       | Publish the port to the host.                       |
| `rocksdb`           | Enable RocksDB storage engine.                      |
| `sql=<url>`         | Load a SQL dump into the database after start.      |
| `opts-file=<path>`  | Merge a my.cnf snippet (path under `configs/`).     |

**Gotchas.** PXC bootstrap: first node has no `master=`. Subsequent nodes use `master=node0,galera`, *or* chain through node1 (`master=node1,galera` on node2/node3) — both are documented patterns. Group Replication: first node has just `group-replication`; later nodes add `master=node0,group-replication`. `opts-file` is silently ignored under `docker-image` mode.

## Percona XtraDB Cluster (PXC)

Inherits all MySQL options above. Cluster-specific:

| Option              | Meaning                                             |
|---------------------|-----------------------------------------------------|
| `cluster=name`      | PXC cluster name (default: `cluster1`).             |
| `galera`            | This node joins the Galera cluster.                 |
| `master=nodeN,galera` | Used on every node *except* the first.            |

## PostgreSQL / Percona PG

| Option              | Meaning                                             |
|---------------------|-----------------------------------------------------|
| `master=nodeN`      | Streaming replica of `nodeN`.                       |
| `primary=nodeN`     | Same as `master=` (PG accepts either form).         |
| `wal=logical`       | Enable logical replication slot.                    |
| `cluster=name`      | Patroni cluster name.                               |
| `standby=nodeN`     | Patroni standby cluster pointing at another cluster. |
| `etcd-ip=nodeN`     | Use `nodeN` as etcd endpoint.                       |
| `clustercheck`      | Enable HAProxy-compatible health endpoint.          |
| `expose=5432`       | Publish to host.                                    |
| `password=<pwd>`    | Override the default DB password.                   |

**Gotchas.** Patroni needs etcd; by default it runs etcd on the first node and others reference it. `standby=nodeN` makes a *standby cluster* (replica of another Patroni cluster) — different from `master=nodeN` which makes a streaming standby of a single node. `repmgr` is a separate keyword on each node, not an option.

## MongoDB / PSMDB

| Option                                       | Meaning                                             |
|----------------------------------------------|-----------------------------------------------------|
| `replica-set=rs0`                            | Join replica set `rs0` (alias: `replica-set:rs0`).  |
| `master=nodeN`                               | Follow the primary on `nodeN`.                      |
| `role=shard` / `shardsrv`                    | Run this mongod as a shard member.                  |
| `role=cfg` / `configsrv`                     | Run this mongod as a config server.                 |
| `mongos-cfg:cfg0/n0,n1,n2`                   | mongos: define which RS is the config server.       |
| `mongos-shard:rs0/n0,n1,n2,rs1/...`          | mongos: list of shard RSes (only on first mongos).  |
| `kerberos-server=nodeN`                      | Enable GSSAPI auth pointing at a KDC on nodeN.      |
| `hostname:rs0-0`                             | Override container hostname (used in CACHING.md style). |

**Gotchas.**
- **Sharded keyfile choreography.** Every replica set in the cluster (each shard, plus cfg) must share **the same keyfile**. Before deploying, generate one and copy:
  ```sh
  openssl rand -base64 756 > secret/cfg0-keyfile
  for k in rs0-keyfile rs1-keyfile rs2-keyfile; do cp -L secret/cfg0-keyfile secret/$k; done
  ```
  The number of `rsN-keyfile` files equals the number of shards. Skip this and `mongos` will refuse to add shards.
- **`mongos-shard:` only on the first mongos.** Additional mongos nodes get `mongos-cfg:` only.
- **`replica-set=` vs `replica-set:`** — both forms work; the equals form is the modern convention.

## Backups

| Option                                       | Meaning                                             |
|----------------------------------------------|-----------------------------------------------------|
| `s3=nodeN/path`                              | MinIO endpoint reference (pgbackrest, PBM).         |
| `repo=/path`                                 | pgbackrest repo path. Default: nfs-client mount if attached, else `/var/lib/pgbackrest`. |
| `source=nodeN`                               | Barman backup source.                               |
| `method=rsync` / `method=streaming-only`     | Barman backup method.                               |

**Gotchas.** `pgbackrest:s3=node0` defaults to bucket name `backup`; `s3=node0/mybucket` overrides. PBM agents must run on every replica set member that should participate in backup (`pbm:latest,s3=node0/backup` on each node). For NFS-shared backup repos, attach `nfs-client:server=node0` on each node — `pgbackrest` then defaults its repo to that mount (`<mount>/pgbackrest`) automatically; pass `pgbackrest:repo=/explicit/path` only to override — see [`topologies.md`](topologies.md).

## PMM (server + client)

| Option (server)                  | Meaning                                             |
|----------------------------------|-----------------------------------------------------|
| `:<version>`                     | PMM server version (e.g. `pmm:3.7.0`, `pmm:2.42.0`). |
| `docker-image`                   | Required modifier — PMM server only ships as a Docker image. |
| `port=12443`                     | Host-side port for the PMM UI.                      |
| `docker-image=<repo/image:tag>`  | Override the image (for dev/nightly builds).        |

| Option (client)                  | Meaning                                             |
|----------------------------------|-----------------------------------------------------|
| `:<version>-<release>`           | E.g. `pmm-client:3.7.0-7`, `pmm-client:2.42.0-6` — release suffix is **mandatory**. |
| `server=node0:8443`              | PMM 3.x server reference (internal port 8443).      |
| `server=node0`                   | PMM 2.x server reference (internal port 443, implicit). |
| `mysql=nodeN` / `pgsql=nodeN`    | Tell pmm-admin which DB to register on this client. |

**Gotchas (re-stating, this is the most-missed thing):**
- PMM **2.x** internal port is **443** → `server=node0`.
- PMM **3.x** internal port is **8443** → `server=node0:8443`.
- The host-side `port=12443` is **not** what the client uses — clients reach the server on the Docker network using its internal port.
- `pmm-client:3.7.0` (no `-7` suffix) **will not resolve.**
- `coroot-client` takes no version. Its options are `server=<node>` (required) plus optional `user=`, `password=`, `port=` to override the database credentials it picks up from the keyword next to it.
- `coroot` takes `port=` for the host-side UI port and `version=` for the image tag (default `latest`). Do not add `,docker-image`, it is implied.

## Object storage (MinIO)

| Option                                       | Meaning                                             |
|----------------------------------------------|-----------------------------------------------------|
| `docker-image`                               | Use upstream MinIO image (recommended).             |
| `expose=9000`                                | Publish S3 port to host.                            |

Reference from clients with `s3=nodeN[/<bucket>]`.

## NFS (new in v0.1.33)

| Option (server)        | Meaning                                             |
|------------------------|-----------------------------------------------------|
| (none — `nfs-server` or `nfs`)| Just the keyword. Exports `/srv/nfs` (tmpfs-backed). |

| Option (client)        | Meaning                                             |
|------------------------|-----------------------------------------------------|
| `server=nodeN`         | Required. Mount source.                             |
| `mount=/path`          | Mount target (default `/mnt/nfs`).                  |

**Gotchas.**
- NFSv4-only with `fsid=0`. Internally the client mounts `server:/`, not `server:/srv/nfs`.
- `showmount` does **not** work (v3 disabled) — use `exportfs -v` on the server.
- The export is **tmpfs**. Data is destroyed with the container.
- **Tear down clients before the server.** A client whose server vanishes leaves a stuck mount that wedges Docker; recovery requires `sudo systemctl restart docker`.

## Auth (LDAP / Kerberos)

| Option                  | Meaning                                             |
|-------------------------|-----------------------------------------------------|
| `ldap-master=default`   | Bind to the `ldap` server on node0.                 |
| `ldap-master=nodeN`     | Bind to the `ldap` server on a specific node.       |
| `kerberos-server=nodeN` | Use `nodeN` as the KDC (PSMDB GSSAPI, PG).          |

## Kubernetes (k3d, operators)

| Option                  | Meaning                                            |
|-------------------------|----------------------------------------------------|
| `nodes=3`               | Number of k3d nodes.                               |
| `cluster-domain=...`    | DNS domain for the cluster.                        |
| `ingress=443`           | Ingress host port.                                 |
| `ingress-type=nginxinc` | Ingress controller flavor.                         |
| `host-alias="IP:h1\|h2"`| Extra `/etc/hosts` entries.                        |
| `namespace=db1`         | k8s namespace to deploy into.                      |
| `replicas=N`            | Operator replica count.                            |
| `shards=N`              | Operator shard count.                              |
| `cluster-name=db1`      | Logical cluster name created by the operator.     |
| `db-version=13`         | DB version the operator should provision.          |
| `helm`                  | Install the operator via Helm.                     |
| `standby`               | Create a standby cluster (k8s-pg).                 |
| `proxysql`              | Enable ProxySQL in the cluster (k8s-pxc).          |
| `certs=self-signed`     | Use self-signed TLS certificates (k8s-minio).      |
| `storage=5Gi`           | PVC size per instance (k8s-cnpg, k8s-crunchy, default `1Gi`). |
| `memory=1Gi`            | Memory request/limit per instance (k8s-cnpg, k8s-crunchy). |
| `sql=path.sql`          | SQL file loaded into the new cluster (k8s-cnpg, k8s-crunchy). |
| `expose`                | LoadBalancer services for the cluster (k8s-crunchy). |
| `bucket=name`           | pgBackRest S3 bucket (k8s-crunchy, default `operator-testing`). |
| `s3=URL`                | External S3 endpoint for pgBackRest, `https://KEY:SECRET@host:port/bucket` (k8s-crunchy). |
| `region=eu-west-1`      | S3 region (k8s-crunchy, default `us-east-1`).      |

**Gotchas.**
- K3D RAM appetite: `cert-manager + operator + 3-replica CR ≈ 6–8 GB`. Underprovisioned hosts will see Pending pods and operator CrashLoopBackOff.
- `cluster-domain=` interacts with ingress and TLS. If you set it, certs must match.
- Operator `db-version=` only takes effect on first deploy; cross-major changes require destroy+redeploy.

**CloudNativePG (`k8s-cnpg`, alias `cnpg`) differences.**
- The operator always runs in `cnpg-system` and watches every namespace, so `namespace=` places the *Cluster*, not the operator. Several `k8s-cnpg` keywords share one operator.
- `replicas=N` is the *total* instance count (1 primary + N-1 replicas), so `replicas=1` is a single node. On the Percona operators it counts replicas of the CR.
- `db-version=17` selects `ghcr.io/cloudnative-pg/postgresql:17`; a full image reference also works.
- PMM and MinIO wiring is skipped with a warning — those patch Percona CR fields a CloudNativePG `Cluster` does not have.

**Crunchy Postgres for Kubernetes (`k8s-crunchy`, alias `crunchy`) differences.**
- Upstream PGO, the operator the Percona PG operator v2 is forked from. Default version `5.8.8`, any tag works (`k8s-crunchy:6.0.2`).
- The operator always runs in `postgres-operator` and watches every namespace, so `namespace=` places the *PostgresCluster*. Several `k8s-crunchy` keywords share one operator.
- `replicas=N` is the *total* instance count, default 3.
- `db-version=17` sets `spec.postgresVersion` and only accepts a major the operator ships an image for (15 to 18 on 5.8 / 6.0). A full image reference sets `spec.image` instead.
- Images pull anonymously from `registry.developers.crunchydata.com`, no account needed. Crunchy tags GitHub before publishing images, so the newest tag may not deploy: anydbver checks the registry and falls back or fails with a clear message. `anydbver versions k8s-crunchy` lists known good ones.
- MinIO backups work: deploy `k8s-minio:latest,certs=self-signed` **and `cert-manager`** alongside, and pgBackRest `repo1` points at the bucket while `repo2` stays a local volume. The replica-create backup goes to `repo1`, so it is populated as soon as the cluster is up.
- TLS on MinIO is not optional: pgBackRest only talks to S3 over HTTPS and fails the stanza on a plain HTTP endpoint. Without `cert-manager` the wiring is skipped with a warning and the cluster keeps its volume repositories.
- `s3=https://KEY:SECRET@host:port/bucket` (plus `bucket=` / `region=`) points pgBackRest at an external S3 instead. An `http://` URL is rejected. Certificate verification is off, these are test buckets.
- `expose` puts `<cluster-name>-ha` (the Patroni leader service) on a LoadBalancer and gives `-replicas` and `-pgbouncer` NodePorts. All three as LoadBalancer would fight over host port 5432 and two would stay Pending. `-primary` is headless and cannot be exposed.
- PMM wiring is still skipped with a warning, it patches Percona CR fields.

## PMM HA (k8s-pmm-ha, Tech Preview)

| Option            | Meaning                                                       |
|-------------------|---------------------------------------------------------------|
| `:VERSION`        | pmm-ha chart version. Default `1.4.1` (PMM 3.7.1).            |
| `size=small`      | Apply `configs/k8s/pmm-ha-small.yaml` so the full stack fits one k3d node (default). |
| `size=full`       | No overrides — chart defaults, requires multi-node cluster.   |
| `replicas=N`      | PMM Server replicas (default 3 for Raft HA).                  |
| `password=...`    | PMM admin password (default `admin`).                         |
| `deps=1.0.0`      | pmm-ha-dependencies (operators umbrella) chart version.       |
| `values=path.yaml`| Extra helm values file applied on top of the small profile.   |
| `namespace=pmm`   | Namespace (default `pmm`).                                    |

**Gotchas.**
- `size=small` is a single-node profile. Real HA needs `size=full` on a multi-node cluster — chart defaults use REQUIRED pod anti-affinity for HAProxy and PostgreSQL.
- ClickHouse stays at 2 replicas even in the small profile: qan-api2 blocks at startup until `system.clusters` has a row with `is_local=0`, so a 1x1 CH cluster deadlocks PMM readiness.
- HAProxy is the only supported external entry: `kubectl port-forward -n pmm svc/pmm-ha-haproxy 8443:443` then browse `https://localhost:8443`.
- Tech Preview — not production-ready, expect breaking changes.

## Valkey

| Option         | Meaning                                                |
|----------------|--------------------------------------------------------|
| `master=nodeN` | Replicate from `nodeN`.                                |
| `sentinel`     | Run Valkey Sentinel on this node.                      |
| `cluster=name` | Valkey cluster name.                                   |

## sysbench

| Option                | Meaning                                             |
|-----------------------|-----------------------------------------------------|
| `mysql=nodeN`         | Target MySQL node (or MySQL Router).                |
| `port=6446`           | Target port (e.g. MySQL Router RW port).            |
| `oltprw`              | Run the OLTP read/write workload.                   |
