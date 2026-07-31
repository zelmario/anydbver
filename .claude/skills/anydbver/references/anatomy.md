# Command anatomy — full reference

> Verified against `anydbver` v0.1.38 / `instructions.md` on 2026-07-31. The CLI's own `anydbver deploy help <keyword>` is canonical when in doubt.

## The shape

```
anydbver [--namespace=<name>] [--keep] [--provider=docker|kubectl] \
         deploy  <node0 items>  node1  <node1 items>  node2  <node2 items>  ...
```

Every `deploy` invocation is a sequence of **node blocks**. A node block is one or more "things to install on that node", separated by spaces.

- `nodeN` (`node1`, `node2`, ..., literal token) switches the following items onto a new node.
- **`node0` is implicit** — anything between `deploy` and the first `nodeN` lands on node0.
- The first item right after a `nodeN` token can also be `node0` written explicitly, with no behavior change. `node0` is only mandatory in advanced mixed forms (e.g. caching examples in CACHING.md/MONGO.md write `default` and `node0` interchangeably).

## Item shape

```
product:version,key1=val1,key2=val2,flag
```

| Separator | Meaning |
|-----------|---------|
| `:` | Between the product and its version. |
| `,` | Between the version and options, and between two options. |
| `=` | Between an option key and its value. |
| space | Next item on the same node, or — if followed by a `nodeN` token — next node. |

Two options share the same key (e.g. `master`) only when they document the same thing (the cross-node reference). Most products document their options in `anydbver deploy help <keyword>`.

## Cross-node references

Cross-node refs use the literal node token. The CLI rewrites it to the container's IP at deploy time (a generic regex pass in `cmd/anydbver/anydbver.go` translates any `extra_*='nodeN'` to the IP).

| Form | Where |
|------|-------|
| `master=node0` | Source/primary on node0 (MySQL, PG, PSMDB, valkey). |
| `master=default` | Same as `master=node0`. `default` is anydbver's alias for node0. |
| `master:default` | Same again — colon form, used in older `install` + `cache:` examples (CACHING.md, MONGO.md). Both colon and equals forms are accepted by the parser. |
| `primary=node0` | Same as `master=` for PG (either form works). |
| `server=node0:8443` | Cross-node reference with port (used by `pmm-client:server=`). The port is the **internal container port** — see PMM gotcha. |
| `s3=node0/bucket` | MinIO endpoint reference (used by `pgbackrest:s3=`, `pbm:s3=`). |
| `kerberos-server=node0` | Point a DB node at a Kerberos KDC running on node0. |
| `nfs-client:server=node0` | Mount the NFS export from node0. |
| `mongos-cfg:cfg0/node6,node7,node8` | mongos config-server replica set definition. The `cfg0` is the RS name; the rest is a comma-separated node list. |
| `mongos-shard:rs0/n0,n1,n2,rs1/...` | mongos shard list — one or more `rsN/n0,n1,n2` groups. **Only on the first mongos**; later mongos nodes get `mongos-cfg:` only. |
| `haproxy-pg:node1,node2,node3` | **Positional**: the "version" slot is reused as a comma-separated node list. Same form for `haproxy-patroni:n1,n2,n3` and `pgbouncer:n1,n2,n3`. |

## Cluster-wide flags vs per-node options

Most options are per-node (live in the item shape). A few are cluster-wide flags after the `deploy` keyword:

| Flag | Meaning |
|------|---------|
| `--namespace=<name>` | Run in an isolated namespace (parallel envs). Default namespace is empty. |
| `--keep` | Add to an existing deployment instead of destroying it first. |
| `--provider=docker` (default) | Use plain Docker. |
| `--provider=kubectl` | Deploy against an already-running k8s cluster. |
| `--cpus=<N>` | Limit each container to N CPUs. |
| `-m <bytes>` | Memory limit per container. |
| `--verbose` | Verbose ansible output. |

These are cluster-wide and go between `anydbver` and `deploy`:

```sh
anydbver --namespace=PS-9999 --keep deploy node2 ps:8.4.5,master=node1
```

## Special keywords (not products)

These shape the deployment but aren't installed on a node:

- `os:<name>` — base OS for the **next** node block. Common values: `el7`, `el8`, `el9`, `el10`, `jammy`, `focal`, `noble`, `bookworm`, `sles15`. Position matters — `os:el8` *before* a product applies to that node. `sles15` is bare-OS only — see [`products.md`](products.md).
- `install <product>` — install steps only, do not start/configure. Used together with `cache:` to bake a reusable image. See [`docker-image-and-cache.md`](docker-image-and-cache.md).
- `cache:<name>` — bake (first deploy) or reuse (subsequent) a pre-built container image keyed by `<name>`.

## `docker-image` modifier

`docker-image` (bare flag) or `docker-image=registry/image:tag` (with a value) tells anydbver to use an unmodified upstream container instead of installing from packages.

What changes:
- No `dnf` / `apt` install.
- No `opts-file` merge into `/etc/<dbconf>` — custom my.cnf-style overlays are **ignored**.
- Faster restart.
- Supported with: `mysql`, `postgresql`, `psmdb` / `mongo`, `valkey`, `minio`, `pmm-server`, `pmm-client`.

```sh
# Bare flag — version is the tag
anydbver deploy valkey:unstable,docker-image

# Full image override
anydbver deploy pmm:docker-image=perconalab/pmm-server:dev-latest,port=12443
```

## Worked examples

```sh
# Default node0 (implicit), single item, latest version
anydbver deploy ps
# → install Percona Server (latest) on node0

# Two nodes, async replication, explicit version
anydbver deploy ps:8.4  node1  ps:8.4,master=node0
# → node0: ps 8.4 source; node1: ps 8.4 replica of node0

# Three items on node0, two items on node1, namespace
anydbver --namespace=ticket-1234 deploy \
  ppg:17 patroni:cluster=cluster1 pgbackrest \
  node1 ppg:17,master=node0 patroni:master=node0,cluster=cluster1 pgbackrest
# → bug-repro env in the ticket-1234 namespace

# Mixed: K3D + cert-manager + PG operator + standby cluster
anydbver deploy k3d k8s-minio:latest,certs=self-signed cert-manager \
  k8s-pg:2.7.0 \
  k8s-pg:2.7.0,namespace=pgo1,standby

# `default` alias + colon form (CACHING.md style)
anydbver deploy install psmdb:4.2.12 cache:psmdb-4.2.12
anydbver deploy \
  default psmdb:4.2.12 replica-set:rs0 \
  node1   psmdb:4.2.12 master:default replica-set:rs0 \
  node2   psmdb:4.2.12 master:default replica-set:rs0
```

## Edge cases

- **`expose=<port>` vs `port=<port>`.** `expose=3306` publishes the container port to the host. `port=12443` is used by PMM/some others to override which **host** port maps to the UI. Different keywords use different conventions — check `deploy help`.
- **`admin-port=9091:9090`** — explicit `host:container` port mapping (used by the operator helm install for an admin port).
- **Quoted values with spaces.** Use double quotes: `host-alias="172.17.0.1:r1.percona.local|r2.percona.local"`. The pipe is part of the value.
- **`,` inside a single value** breaks the parser (it's the option separator). Avoid CSV-style values; use space-delimited keywords or wrap with another product. The `mongos-shard:` and positional `haproxy-pg:` forms work because the parser specifically supports comma-list in the version slot.
