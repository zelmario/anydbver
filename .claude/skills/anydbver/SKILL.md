---
name: anydbver
description: Drive `anydbver` to spin up multi-node database test environments as Docker containers (or nested K3D clusters) — Percona Server / MySQL / MariaDB / PXC, PostgreSQL / Percona PG, PSMDB / MongoDB, Valkey, plus the surrounding stack (Patroni, repmgr, ProxySQL, Orchestrator, HAProxy, pgbouncer, pgbackrest, Barman, PBM, xtrabackup, PMM 2.x/3.x, PMM HA Tech Preview on k8s, MinIO, NFS, LDAP, Kerberos, sysbench, Percona and CloudNativePG operators on K3D). Use when the user wants to spin up Mongo locally, armar una réplica de Postgres, build a 3-node PXC cluster, set up Patroni HA, reproducir un bug en MySQL 8.4 / PSMDB 8.0, run local k8s with the Percona PG or CloudNativePG operator, levantar PMM monitoreando varias DBs, reproducir un ticket de PMM HA, or tirar abajo el ambiente. Also drives the experimental `chaos` command to inject network faults (latency / loss / partition / node kill) into a deployed environment for HA / failover testing. Triggers on `anydbver`, `psmdb:`, `ps:`, `pxc:`, `pg:`, `ppg:`, `pgbackrest:s3=`, `mongos-shard`, `replica-set=`, `role=shard`, `master=node0`, `master=default`, `kerberos-server=`, `nfs-client:server=`, `pmm-client:3.7.0-7,server=node0:8443`, `k3d:`, `k8s-pg`, `k8s-cnpg`, `k8s-pxc`, `k8s-psmdb`, `k8s-pmm-ha`, `pmm-ha:`, `cache:`, `chaos`, `chaos link`, `chaos partition`, `chaos ui`. This skill does **not** apply to production deployments, managed services (Atlas / RDS / CloudSQL / Cosmos), or diagnosing problems in already-running systems unrelated to anydbver.
---

# anydbver

`anydbver` is a CLI for **short-lived, throwaway** database test environments. You write the topology on one command line; it runs Ansible inside Docker containers (or k3d for k8s topologies). It is the right tool for reproducing bugs on a specific product+version+topology, trying a new HA layout, comparing versions, or smoke-testing operators — and the wrong tool for anything you want to keep running.

## When to reach for this skill

- The user asks to spin up a database or cluster locally (`spin up Mongo`, `armar un PXC`, `levantar Patroni`).
- An issue or customer ticket needs reproducing on a specific product + version + topology — bug-repro is the canonical use case.
- The user wants a multi-DB lab (`PMM monitoring MySQL + PG + Mongo at the same time`).
- The user wants to try a Percona operator locally on K3D before touching real Kubernetes.
- The user references `anydbver`, any of the keyword tokens above, or asks to tear down (`anydbver destroy`, `tirar abajo`, `limpiar el ambiente`).

## What this skill does NOT cover

- **Production / persistent deployments.** anydbver clusters are throwaway by design — tmpfs-backed in places, no upgrade story.
- **Managed services** (Atlas, RDS, CloudSQL, Cosmos DB). Different tool, different problem.
- **Diagnosing an already-running issue** unrelated to anydbver itself. Use the relevant DB tooling directly.
- **Raw `docker run` flows.** anydbver is the abstraction; the underlying containers are not a stable interface.

## Prerequisites checklist

Before composing a deploy, run these in order:

1. `anydbver --version` — confirms binary present (current stable: v0.1.35).
2. `docker version` and group membership: `id -nG | grep -q docker || echo "user not in docker group"`.
3. `anydbver list` — shows what's already running. **A bare `anydbver deploy` (no `--keep`) implicitly destroys whatever is in the current namespace** — confirm with the user before stepping on someone else's environment.
4. `anydbver update` — refreshes the local version DB if versions look wrong or the user just upgraded.

## Introspection-first principle

The instructions and tables below were verified on **2026-06-04**. anydbver evolves; the CLI's own help is canonical. Before emitting a non-trivial deploy command for a keyword combination you have not used in the conversation:

```sh
anydbver deploy help <keyword>      # subcommands + verbatim CLI examples
anydbver deploy help keywords       # full keyword list
anydbver test list                  # every built-in recipe
```

Prefer the CLI's output to a memorized table when there is any doubt. See [`references/options.md`](references/options.md) for the full per-family option tables.

## Command anatomy

```
anydbver deploy  <node0 items>  node1  <node1 items>  node2  <node2 items>  ...
```

- A **node block** is one or more "things to install on that node", separated by spaces.
- `nodeN` (literal token: `node1`, `node2`, ...) switches the following items onto a new node.
- **`node0` is implicit** — anything between `deploy` and the first `nodeN` lands on node0.
- Each item has the shape `product:version,key1=val1,key2=val2,flag`.

| Separator | Meaning |
|-----------|---------|
| `:` | between product and version |
| `,` | between version and options, and between options |
| `=` | between an option key and its value |
| space | next item on the same node, or (when followed by `nodeN`) next node |

Cross-node references use the node literal (`master=node0`, `server=node0:8443`, `s3=node0/bucket`, `kerberos-server=node1`, `nfs-client:server=node0`). The shorthand **`default` means `node0`** (used in `master=default`, `ldap-master=default`). The colon form **`master:default`** appears in older `install` + `cache:` examples (MONGO.md, CACHING.md) and is parsed equivalently — both forms work, but if a single command mixes them, prefer one for consistency.

For the painful details (every separator, the positional `haproxy-pg:n1,n2,n3` "version slot as node list" form, what `docker-image` changes), see [`references/anatomy.md`](references/anatomy.md).

## Quick reference

```sh
# Single node
anydbver deploy ps                                      # latest Percona Server
anydbver deploy pg:18                                   # PostgreSQL 18
anydbver deploy psmdb:8.0                               # PSMDB 8.0

# Async / streaming replication
anydbver deploy ps:8.4 node1 ps:8.4,master=node0
anydbver deploy node0 pg:18,wal=logical node1 pg:18,primary=node0,wal=logical

# 3-node PXC 8.4 Galera
anydbver deploy pxc:8.4 node1 pxc:8.4,master=node0,galera node2 pxc:8.4,master=node0,galera

# 3-node PSMDB 8.0 replica set
anydbver deploy psmdb:8.0,replica-set=rs0 \
  node1 psmdb:8.0,replica-set=rs0,master=node0 \
  node2 psmdb:8.0,replica-set=rs0,master=node0

# Patroni HA (3 nodes, vanilla PG)
anydbver deploy pg patroni \
  node1 pg:master=node0 patroni:master=node0 \
  node2 pg:master=node0 patroni:master=node0

# PMM 3.x server + monitored PS node (port distinction matters!)
anydbver deploy pmm:3.7.0,docker-image,port=12443 \
  node1 ps:latest pmm-client:3.7.0-7,server=node0:8443

# Bug repro in a dedicated namespace
anydbver --namespace=PS-9999 deploy ps:8.4.5 node1 ps:8.4.5,master=node0

# K3D + PXC operator
anydbver deploy k3d k8s-pxc:1.17.0

# Tear down current namespace; namespaces let you run several side by side
anydbver list
anydbver --namespace=PS-9999 destroy
```

The full library of real, copy-pasteable examples by scenario lives in [`references/examples.md`](references/examples.md).

## Workflows

Each is self-contained: prereq → command → expected layout → verification → teardown. Pick the closest match and adapt — see the linked reference for the recipe.

- **A. Single-node deploy** of any DB. → [`references/topologies.md#single-node`](references/topologies.md#single-node)
- **B. Async / streaming replication** (MySQL source+replica, PG primary+standby, PSMDB replica set). → [`references/topologies.md#replication`](references/topologies.md#replication)
- **C. Multi-node clusters** — PXC/Galera, MariaDB Galera, MySQL Group Replication + Router, PSMDB sharded, Patroni HA. → [`references/topologies.md#clusters`](references/topologies.md#clusters)
- **D. Reproduce a customer bug** at an exact product+version+topology. **Default to `--namespace=<ticket-id>`** so the repro is isolated and side-by-side with whatever else is running. → [`references/examples.md#bug-repro`](references/examples.md#bug-repro)
- **E. Backups** — pgbackrest (filesystem or S3/MinIO), PBM (filesystem or S3), Barman, xtrabackup. → [`references/topologies.md#backups`](references/topologies.md#backups)
- **F. Monitoring with PMM** — see *PMM 2.x vs 3.x* under gotchas. → [`references/examples.md#pmm`](references/examples.md#pmm)
- **G. Operators on K3D** — k8s-ps, k8s-pxc, k8s-pg (incl. standby clusters), k8s-cnpg (CloudNativePG), k8s-psmdb, k8s-minio. → [`references/topologies.md#operators`](references/topologies.md#operators)
- **H. Multi-DB / cross-product** (one PMM monitoring three DBs; side-by-side via `--namespace=`). → [`references/examples.md#multi-db`](references/examples.md#multi-db)
- **I. Cleanup and namespaces** — `--keep` for additive growth, `--namespace=` for parallel envs, `destroy --remove-cache`. → [`references/examples.md#environment`](references/examples.md#environment)
- **J. `install` + `cache:`** — pre-bake images for repeated deploys; great for bug-repro loops. → [`references/docker-image-and-cache.md`](references/docker-image-and-cache.md)
- **K. Network chaos** — degrade / partition / kill nodes of an already-deployed cluster to test HA, failover, split-brain, leader re-election. → see *Chaos* below.

## Chaos — network fault injection (experimental)

Operates on an **already-deployed** environment — no redeploy. `chaos` is a **hidden** command (not in `anydbver --help`); use `anydbver chaos --help`. Faults are applied with `tc`/`netem` **inside each container's netns** via a throwaway helper container (default image `gaiadocker/iproute2`, override `ANYDBVER_TC_IMAGE`), so it works on native Linux **and** Docker Desktop / WSL2 with no host `sudo`.

```sh
anydbver chaos link node0 node1 delay=120ms loss=5%   # symmetric degrade
anydbver chaos partition node0 node1                  # 100% loss both ways
anydbver chaos pause node1                            # also unpause / kill / start
anydbver chaos status                                # current shaping per node
anydbver chaos measure node0 node1                   # real RTT + one-way + loss
anydbver chaos clear                                 # remove all faults
anydbver chaos ui                                    # topology dashboard @ :8080
```

`chaos link` params (each also accepts a `min-max` range): `delay= jitter= loss= corrupt= dup= reorder= rate= corr=`.

- **Links are symmetric → RTT ≈ 2× `delay`** (each one-way trip pays the delay once). Use `chaos measure` for the real numbers; one-way ≈ RTT/2 is the apples-to-apples comparison to the induced per-direction delay.
- **Dead-man switch:** every fault auto-clears after `--ttl` seconds (default 3600; `--ttl 0` disables). The `ui` clears on exit and on an inactivity timer.
- **Teardown is safe:** `anydbver destroy` auto-unpauses any chaos-paused node and tc shaping dies with the container's netns — no manual `chaos clear` needed before destroy.
- The dashboard adds click-to-degrade, multi-select (Ctrl/⌘-click), link flapping, flux (drifting ranged values), and a chaos monkey.

## Decision points worth stopping on

- **Bare-Docker vs operator-on-K3D.** Operators only when the user is testing operator behavior; bare-Docker is faster, smaller, easier to introspect.
- **PSMDB vs MongoDB Community.** Use PSMDB unless the user explicitly needs Community semantics — then `mongo:X,docker-image`.
- **Single vs replica set vs sharded** for Mongo — sharded is 9+ nodes and a keyfile choreography; only when the bug actually needs it.
- **Async vs PXC vs Group Replication** for MySQL — async = simplest; PXC = synchronous Galera; GR = InnoDB Cluster + Router.
- **Patroni vs repmgr vs plain streaming** for PG — Patroni for failover testing, repmgr for replication-manager workflows, streaming for the simplest standby.
- **haproxy-pg vs haproxy-patroni vs pgbouncer** — first two are HAProxy variants (pick one based on what the cluster type is); pgbouncer is a connection pooler that goes in front of either.
- **pgbackrest vs Barman** — pgbackrest is push-style and supports S3 directly; Barman is pull-style.
- **Filesystem vs S3 (MinIO) backup storage** — S3/MinIO when you want to test cloud-shaped paths; filesystem (or NFS) when you don't.
- **PMM 2.x vs 3.x** — see gotchas. **Always pin** when reproducing a bug.
- **Fresh deploy vs `cache:<name>`** — cache for the second+ time you deploy the same combo (bug-repro loop, repeated tests). Fresh otherwise.

## Critical gotchas

- **`master=default` ≡ `master=node0`.** anydbver's shorthand. Same for `ldap-master=default`. The colon form `master:default` (used in older `install`/`cache:` examples) is parsed equivalently.
- **PXC bootstrap.** First node has no `master=`; subsequent nodes use `master=node0,galera` (or chain through node1).
- **MySQL Group Replication.** First node has just `group-replication`; others add `master=node0,group-replication`. Add a `mysql-router` node if you need a routed entry point.
- **PSMDB sharded keyfile.** Every replica set must share the **same keyfile**. Before deploying, copy the cfg keyfile to every shard: `for k in rs0-keyfile rs1-keyfile rs2-keyfile; do cp -L secret/cfg0-keyfile secret/$k; done`. Skip and the `mongos` will refuse to add shards.
- **`mongos-shard:` only on the first mongos.** Subsequent mongos nodes get `mongos-cfg:` only.
- **PMM 2.x vs 3.x ports.** PMM **2.x** internal port is 443 → `pmm-client:2.42.0-6,server=node0` (no port). PMM **3.x** internal port is 8443 → `pmm-client:3.7.0-7,server=node0:8443`. The host-side `port=12443` (or whatever) is unrelated to what the client reaches; clients use the **internal** port on the Docker network.
- **`pmm-client` carries a release suffix.** `pmm-client:3.7.0-7`, `pmm-client:2.42.0-6` — bare `pmm-client:3.7.0` will not resolve.
- **Re-running `deploy` without `--keep` implicitly destroys** the previous deployment in the **same namespace**. Always show `anydbver list` before such a deploy if anything could be in flight.
- **`anydbver destroy` is per-namespace.** Bare `destroy` only clears the current namespace; to wipe everything iterate `anydbver namespace list`.
- **NFS destroy order.** Tear down clients before the server. A client whose server vanishes leaves a stuck mount that wedges Docker; only `sudo systemctl restart docker` clears it.
- **`docker-image` mode skips package install** and any `opts-file` merge — fast, but custom config files are silently ignored.
- **Stale `ANSIBLE_VERSION`.** If a recently-added role or playbook change isn't taking effect, the binary's `ANSIBLE_VERSION` in `pkg/common/images.go` may be older than what's published; running from a source tree avoids this (it mounts local roles), binary-only users need to upgrade `anydbver`.

## References

Loaded on demand — open the file the user's question is closest to.

- [`references/anatomy.md`](references/anatomy.md) — every separator, every cross-node reference form, edge cases (`default` vs `node0`, `master:default` vs `master=default`, positional `haproxy-pg:n1,n2,n3`).
- [`references/products.md`](references/products.md) — canonical product table with every alias.
- [`references/options.md`](references/options.md) — every option per family (mysql / pg / mongo / k8s / nfs / etc.) with per-family gotcha paragraphs. **The CLI's `deploy help <keyword>` is canonical** — these tables are dated.
- [`references/examples.md`](references/examples.md) — every "Examples by scenario" block from `instructions.md`, plus PMM, multi-DB, NFS, bug-repro patterns.
- [`references/topologies.md`](references/topologies.md) — recipes per topology family with deploy / layout / verification / "use this when".
- [`references/docker-image-and-cache.md`](references/docker-image-and-cache.md) — `docker-image` mode, `install` + `cache:` system, optional nginx package-cache proxy.
- [`references/troubleshooting.md`](references/troubleshooting.md) — diagnostic kit + symptom-organized failures.

## Output expectations

When the user asks to deploy or operate on anydbver nodes, produce:

1. **One-line prereq confirmation** — `anydbver --version`, Docker reachable, namespace status (or that you're using a fresh `--namespace=<name>`).
2. **The exact `anydbver deploy ...` command** with realistic versions. **Never use `<version>` placeholders.** Pin explicitly for bug repros (e.g. `ps:8.4.5`, not `ps:latest`); for general use, the cheatsheet defaults are fine.
3. **Expected node layout** — what runs where (`node0: ps:8.4.5 source`, `node1: ps:8.4.5 replica`), namespace, host port mappings if any.
4. **Verification step** — concrete `anydbver exec ... -- <client>` invocation, or `pbm status`, `pmm-admin list`, `kubectl get psmdb`, `exportfs -v` — whatever proves the thing works.
5. **Teardown command** — namespace-scoped if a namespace was used: `anydbver --namespace=<name> destroy`.

**Destroy safety.** Never run `anydbver destroy` — and never trigger an implicit destroy by re-running `deploy` without `--keep` against a populated namespace — without first showing `anydbver list` and confirming with the user. Exception: the user has explicitly asked to wipe in the current turn.
