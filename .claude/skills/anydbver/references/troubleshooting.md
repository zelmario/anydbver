# Troubleshooting

> Verified on 2026-05-07. Symptom-organized; first run the diagnostic kit, then jump to the matching family.

## Always-run-first diagnostic kit

```sh
anydbver --version                              # is the binary recent? (current stable: v0.1.33)
docker version                                  # is Docker reachable?
id -nG | grep -q docker && echo OK || echo "user not in docker group"
anydbver list                                   # what's in the current namespace?
anydbver namespace list                         # any other namespaces alive?
docker image ls | grep -E 'anydbver-ansible|anydbver-cache' | head
docker network ls | grep anydbver
docker logs <container-id> 2>&1 | tail -50      # for a specific node
echo 'journalctl -xe --no-pager | tail -100' | anydbver exec node0 -- bash -il
```

## General

### "Cannot connect to Docker daemon"
User not in the `docker` group, or Docker daemon not running.
```sh
sudo systemctl status docker
sudo usermod -aG docker $USER && newgrp docker
```

### "Unknown keyword" / "Unknown subcommand"
Misspelling or stale version DB.
```sh
anydbver deploy help keywords        # canonical list
anydbver deploy help <keyword>       # subcommands for that keyword
anydbver update                      # refresh local version DB
```

### Version not found
`ps:8.4.5` resolves but `ps:8.4.99` doesn't. The local version DB at `~/.config/anydbver/anydbver_version.db` may be older than what you expect.
```sh
anydbver update
sqlite3 ~/.config/anydbver/anydbver_version.db "SELECT version FROM percona_server_version WHERE version LIKE '8.4%' ORDER BY version DESC LIMIT 5"
```

### Container OOM / "killed"
K3D + cert-manager + an operator + a 3-replica CR ≈ 6–8 GB. Lower the topology or raise host RAM. For a single container, `--memory=` and `-m` flags exist.

### Port conflict
`Error: bind: address already in use`. Pick a different host port:
```sh
anydbver deploy ps:latest,expose=3307           # not 3306
anydbver deploy pmm:3.7.0,docker-image,port=12444   # not 12443
```

### Namespace collision
Re-running `deploy` without `--keep` against the **same** namespace destroys the previous deploy first. Use `--namespace=<unique>` to avoid stepping on someone else's environment. To wipe everything, iterate `anydbver namespace list`.

### `docker-image` ignores `opts-file`
By design — `docker-image` mode skips package install AND any `opts-file` merge. If you need `/etc/my.cnf.d/...` overlays, drop the `docker-image` modifier.

### Stale `ANSIBLE_VERSION`
A recently-added role or playbook change isn't taking effect. The compiled-in `ANSIBLE_VERSION` (in `pkg/common/images.go`) determines which ansible Docker image the binary pulls; if a newer image was published but the user's binary is older, the new role isn't seen.

```sh
anydbver --version                                    # check current binary version
docker image ls | grep anydbver-ansible | head -3     # what's pulled locally
```

If running from the source tree (developer setup): `runPlaybook()` mounts local `roles/` and `playbook.yml` into the container, so source-tree changes apply immediately. Binary-only users need to `wget` a newer release of `anydbver`.

### "command not found: anydbver" after a fresh install
`~/.local/bin` is not on PATH. Either log out + log in, or `export PATH="$HOME/.local/bin:$PATH"` in your shell rc.

## MySQL / Percona Server / PXC

### PXC node won't join the cluster
- **First node has no `master=`.** That's the bootstrap. Subsequent nodes need `master=node0,galera` (or chain through node1 with `master=node1,galera`).
- Check SST credentials: `anydbver exec node1 -- grep wsrep_sst /etc/my.cnf`
- Check `wsrep_cluster_address` resolves to running peers.
- Galera cache (`gcache.size`) may be too small if the node has been down a while — falls back to SST. SST is slow on large datasets.
- Logs: `anydbver exec node1 -- tail -200 /var/log/mysqld.log`

### Async replication broken
```sh
echo 'show replica status\G' | anydbver exec node1 -- mysql
```
Look for `Last_IO_Error` / `Last_SQL_Error`. Common: GTID set divergence, missing user, network ACLs (containers in same anydbver network, but still).

### ProxySQL not routing
- Verify mysql_servers / mysql_users are populated: `anydbver exec node2 -- mysql -h127.0.0.1 -P6032 -uadmin -padmin -e 'select * from mysql_servers'`
- ProxySQL container needs to know about backends — for `percona-proxysql:latest,master=node0`, the `master=` populates the source.

### MySQL Router can't reach the cluster
GR cluster must be initialized **first** before MySQL Router bootstraps against it. Re-deploy with the GR options on node0/node1, then `mysql-router` on node2 (which is the documented order).

## PostgreSQL / Patroni / repmgr

### Standby not catching up
Check (in order): replication slot exists on the primary, WAL receiver is running on the replica, `pg_hba.conf` allows the replication user, `wal_level >= replica` (or `logical` if you asked for it).
```sh
echo 'select * from pg_replication_slots'        | anydbver exec node0 -- psql -U postgres
echo 'select * from pg_stat_wal_receiver'        | anydbver exec node1 -- psql -U postgres
anydbver exec node0 -- cat /var/lib/pgsql/data/pg_hba.conf | grep replication
```

### Patroni: DCS unreachable / split-brain
Patroni uses etcd by default, started on the first node. Check etcd:
```sh
anydbver exec node0 -- systemctl status etcd
echo 'patronictl list' | anydbver exec node0
```
If two nodes both claim leader, the deploy probably came up wrong — destroy + redeploy.

### pgbackrest stanza issues
```sh
anydbver exec node0 -- pgbackrest --stanza=main info
```
Common: stanza not created (`pgbackrest --stanza=main stanza-create`), `repo1-path` mismatch with `pgbackrest:repo=...`, S3 endpoint not reachable from the container (verify with `curl https://<minio-ip>:9000`).

## MongoDB / PSMDB

### Replica set initialization hangs
First member missing `replica-set=rs0`, or members can't see each other. Check:
```sh
echo 'rs.status()'                | anydbver exec node0 -- mongosh
echo 'rs.config()'                | anydbver exec node0 -- mongosh
anydbver exec node0 -- ps -ef | grep mongod
```
If hostnames are unresolvable from inside containers, verify the docker network: `docker network inspect <namespace>-anydbver`.

### Sharded cluster: keyfile mismatch
**Most common sharded-cluster failure.** Every replica set must share the same keyfile. Symptom: `mongos` can't add shards, errors like `mismatched cluster IDs`. Recovery:
```sh
# Destroy first — keyfiles are baked into volumes
anydbver --namespace=<name> destroy

# Regenerate and copy
openssl rand -base64 756 > secret/cfg0-keyfile
for k in rs0-keyfile rs1-keyfile rs2-keyfile; do cp -L secret/cfg0-keyfile secret/$k; done

# Redeploy
anydbver --namespace=<name> deploy ...
```

### `mongos` cannot add shards
Beyond the keyfile (above), check that:
- Each shard RS is fully up (`rs.status()` on a shard primary).
- The `mongos-cfg:cfg0/...` and `mongos-shard:rs0/...,rs1/...` lists name **node hostnames** that resolve inside the container network.
- `mongos-shard:` is **only on the first mongos**.

### PBM agent not running
```sh
anydbver exec node1 -- systemctl status pbm-agent
anydbver exec node1 -- pbm status
```
PBM needs an agent on every RS member that should participate in backup. The deploy line should include `pbm:latest,s3=node0/backup` on every node, not just one.

### PMM client unreachable
- Wrong port for PMM version: PMM 3.x → `server=node0:8443`, PMM 2.x → `server=node0`. See SKILL.md gotchas.
- `pmm-client` version is missing the release suffix: needs `:3.7.0-7`, not `:3.7.0`.
- `pmm-admin list` from inside the client node will show registered services.

## Kubernetes (K3D)

### Pods Pending
Insufficient host resources. K3D nodes are nested Docker — RAM/CPU on the host has to fit cert-manager + operator + DB CR. Free up resources or shrink `replicas=`.

### Operator CrashLoopBackOff
```sh
kubectl get pods -A
kubectl logs -n <ns> <operator-pod> --previous
```
Common: cert-manager not ready when operator started (race), missing CRDs (`kubectl get crd | grep percona`), helm vs non-helm mismatch on second deploy.

### ImagePullBackOff
- Operator needs internet from inside the K3D nodes.
- Helm install may pull from a different registry than the non-helm path.

### Resource shortage in K3D
`docker stats` on the host. Bump host RAM, lower `replicas=`, drop `cert-manager` if you don't need TLS.

## NFS (new in v0.1.33)

### `exportfs: /srv/nfs does not support NFS export`
Overlayfs cannot be NFS-exported. The role works around this by mounting tmpfs on `/srv/nfs` before exporting. If you see this error, the role version may be older than v0.1.33 — `anydbver --version`.

### Client mount fails: "No such file or directory"
NFSv4 needs `fsid=0` for clean access; the client mount source must be `server:/`, not `server:/srv/nfs`. The role handles this; manual reproductions need the same.

### `anydbver destroy` hangs after NFS deploy
**Tear down clients before the server.** A client whose server vanishes leaves a stuck mount that the kernel can't unmount; Docker can't kill the container. Recovery: `sudo systemctl restart docker`.

### `showmount -e` fails
v3 is disabled (`vers3=n` in `/etc/nfs.conf`); rpcbind/mountd are not listening. Use `exportfs -v` on the server instead.

## When to fix in place vs `destroy + redeploy`

| Symptom | Fix in place? |
|---------|---------------|
| Misspelled keyword | No — re-run deploy (it'll destroy and redeploy) |
| Wrong version (typo) | No — same |
| Replica failed to start, others fine | Maybe — `anydbver --keep deploy node1 ...` if the topology is fine, otherwise destroy |
| Sharded keyfile mismatch | **No.** Keyfiles are persisted in `secret/`; regenerate, copy, then destroy + redeploy. |
| Patroni split-brain | No — destroy + redeploy. |
| One node OOM-killed | Maybe — bump memory, redeploy that node with `--keep`. |
| Operator CrashLoopBackOff after deploy | Investigate first; many cases are config-fixable via `kubectl edit`. If stuck, `destroy --remove-cache`. |

**Always confirm a `destroy` with the user before running it** — see SKILL.md "Destroy safety" rule.
