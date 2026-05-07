# `docker-image` mode + `install` / `cache:` system

> Verified on 2026-05-07 against `instructions.md`, `CACHING.md`.

## `docker-image` modifier

By default anydbver installs from OS packages (mimics a real-machine deploy). Add `docker-image` as a flag (or `docker-image=registry/image:tag` with a value) to use an unmodified upstream container instead.

```sh
# Bare flag — version is the tag
anydbver deploy valkey:unstable,docker-image \
  node1 valkey:unstable,docker-image,master=node0 \
  node2 valkey:unstable,docker-image,master=node0

# Explicit image and tag
anydbver deploy pmm:docker-image=perconalab/pmm-server:dev-latest,port=12443 \
  node1 mysql:latest,docker-image \
  node2 pmm-client:docker-image=perconalab/pmm-client:dev-latest,server=node0,mysql=node1

# MinIO is almost always docker-image
anydbver deploy minio:docker-image  node1 pg pgbackrest:s3=node0
```

**Supported with `docker-image`:** `mysql`, `postgresql`, `psmdb` / `mongo`, `valkey`, `minio`, `pmm-server`, `pmm-client`.

**What `docker-image` changes:**
- Skips the `dnf` / `apt` package install entirely.
- Skips the `opts-file` overlay merge into `/etc/<dbconf>` — **custom config snippets are silently ignored**. If you need a custom `my.cnf`, drop `docker-image` and use the package install path.
- Faster restart and fully reproducible (no package mirror state).
- The value of `docker-image=` can be a full `registry/image:tag`. Bare `docker-image` (no value) reuses the keyword's `version` field as the tag — only useful when the version string is a valid Docker tag.

**When to use it:**
- You explicitly want unmodified upstream behavior (testing a CVE on stock MySQL).
- You're using a dev/nightly image (`perconalab/pmm-server:dev-latest`).
- Speed: rebuild loops where you don't need the package-install path.

**When not to:**
- Custom my.cnf / postgresql.conf / mongod.conf via `opts-file`.
- Anything that depends on systemd services (the unmodified upstream image runs the binary directly).

## `install` + `cache:` system

For repeated deployments of the same software stack you can pre-bake a container image and reuse it. Two keywords drive this:

- `install <product>` — run install steps only, do **not** start/configure the service.
- `cache:<name>` — bake the image (first deploy) or reuse it (subsequent deploys), keyed by `<name>`.

The pattern is **two passes**: build the cache once, then reference it across many nodes.

```sh
# 1) Build a cache called "ps-8.4.5"
anydbver deploy install ps:8.4.5 cache:ps-8.4.5

# 2) Reuse it across all nodes of a real deployment
anydbver deploy \
          install ps:8.4.5 cache:ps-8.4.5 \
  node1   install ps:8.4.5 cache:ps-8.4.5 \
  node2   install ps:8.4.5 cache:ps-8.4.5 \
  default ps:8.4.5 \
  node1   ps:8.4.5 master:default \
  node2   ps:8.4.5 master:default
```

The double `install ... cache:` line per node is real: the first invocation pulls/uses the cache image, the second runs the actual configure-and-start step.

**Supported with `install`:** `mysql`, `percona-server`, `psmdb`, `mariadb`, `postgresql`, `patroni`, `pmm-server`, samba, `k3s`. (Per CACHING.md.)

**Examples from CACHING.md:**

```sh
# k3s cache for repeated operator deploys
anydbver deploy install k3s cache:k8s
anydbver deploy \
        install k3s cache:k8s \
  node1 install k3s cache:k8s \
  node2 install k3s cache:k8s \
  node3 install k3s cache:k8s \
  default k3s \
  node1 k3s-master:default \
  node2 k3s-master:default \
  node3 k3s-master:default \
  default k8s-pmm k8s-pxc

# PG 13 + Patroni
anydbver deploy install pg:13 patroni cache:pg13-patroni
anydbver deploy \
          install pg:13 patroni cache:pg13-patroni \
  node1   install pg:13 patroni cache:pg13-patroni \
  node2   install pg:13 patroni cache:pg13-patroni \
  default pg:13 patroni \
  node1   pg:13 master:default patroni etcd-ip:default \
  node2   pg:13 master:default patroni etcd-ip:default

# k3s server + worker caches with timing
# 6m32s cached vs 10m3s without
anydbver deploy cache:k3s-master install k3s node1 cache:k3s-worker install k3s-master:default
anydbver deploy \
  node0 cache:k3s-master \
  node1 cache:k3s-worker \
  node2 cache:k3s-worker \
  node3 cache:k3s-worker \
  node0 k3s \
  node1 k3s-master:node0 \
  node2 k3s-master:node0 \
  node3 k3s-master:node0 \
  node0 k8s-pxc
```

**`master:default` (colon).** This is the older `install`/`cache:` form. The parser accepts both `master:default` and `master=default`; `default` itself is the `node0` alias. If a single command mixes both forms, prefer one for consistency.

**When this pays off:**
- Bug-repro loops where you destroy + redeploy the same stack many times.
- Multi-node deployments where every node would otherwise run the same package-install steps.

**Cache cleanup.** `anydbver destroy --remove-cache` wipes cache images alongside containers. Cache images live as Docker images (`docker image ls | grep anydbver-cache`).

## Optional: nginx package proxy cache

For deploys on a slow internet connection, you can run a local nginx proxy that caches Percona / MariaDB package downloads. Per CACHING.md:

```nginx
# /etc/nginx/conf.d/anydbver-cache.conf (excerpt)
proxy_cache_path /mnt/data/nginx-cache levels=1:2 keys_zone=my_cache:10m max_size=10g inactive=10080m use_temp_path=off;

server {
    listen 80;
    server_name repo.percona.com.local;
    location / { proxy_cache my_cache; proxy_pass http://repo.percona.com; }
}

server {
    listen 80;
    server_name downloads.mariadb.com.local;
    location / { proxy_cache my_cache; proxy_pass https://downloads.mariadb.com;
                 proxy_set_header Host downloads.mariadb.com; proxy_ssl_server_name on; }
}
```

Then on the LXD/Docker host:

```text
ip_of_nginx_server  repo.percona.com.local
ip_of_nginx_server  downloads.mariadb.com.local
```

…in `/etc/hosts`, plus `export LOCAL_REPO_CACHE=1` (add to `~/.bashrc`).

## OS-image cache

```sh
export ANYDBVER_CACHE_OS_IMG=1
```

First deploy caches the base OS image as `${USER}-$OS-empty`; subsequent deploys for the same OS reuse it. Reduces container startup latency.

## Decision rules

| Situation | Use |
|-----------|-----|
| Single deploy, throwaway | Plain `anydbver deploy` |
| Repeated deploys of the same stack | `install` + `cache:` |
| Need unmodified upstream image | `docker-image` |
| Need custom `my.cnf` etc. | Plain (no `docker-image`) — the `opts-file` overlay only runs in package mode |
| Slow internet, repeated installs | nginx proxy + `LOCAL_REPO_CACHE=1` |
| Fast container restart, same OS | `ANYDBVER_CACHE_OS_IMG=1` |
