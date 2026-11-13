package unmodifieddocker

import (
	"fmt"
	"html"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"time"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

// ClickHouseNode is one docker-image ClickHouse node of the deployment.
type ClickHouseNode struct {
	Name   string // node0, node1, ...
	Master string // its master= node, "" for a Keeper root
	Shard  int
}

var clickHouseNodes []ClickHouseNode

// SetClickHouseTopology records every docker-image ClickHouse node, in node
// order, before the containers are created, so each one can list its whole
// cluster.
func SetClickHouseTopology(nodes []ClickHouseNode) {
	clickHouseNodes = nodes
}

// clickHouseMembers returns the nodes that share the Keeper root, in node
// order. Without a recorded topology the node is alone.
func clickHouseMembers(root, name, shard string) []ClickHouseNode {
	members := []ClickHouseNode{}
	for _, n := range clickHouseNodes {
		r := n.Name
		if n.Master != "" {
			r = n.Master
		}
		if r == root {
			members = append(members, n)
		}
	}
	if len(members) == 0 {
		s, _ := strconv.Atoi(shard)
		members = append(members, ClickHouseNode{Name: name, Shard: s})
	}
	return members
}

// CreateClickHouseContainer starts the official clickhouse/clickhouse-server
// image. Nodes find each other by container name: the --hostname is not
// resolvable from the other containers.
func CreateClickHouseContainer(logger *log.Logger, namespace string, name string, cmd string, args map[string]string) {
	tools_dir := anydbver_common.GetToolsDirectory(logger, namespace)
	self := anydbver_common.MakeContainerHostName(logger, namespace, name)

	cmd_args := []string{
		"docker", "run",
		"--name", self,
		"--platform", "linux/" + runtime.GOARCH,
		"-v", filepath.Dir(anydbver_common.GetConfigPath(logger)) + "/secret:/vagrant/secret",
		"-v", anydbver_common.GetCacheDirectory(logger) + "/data/nfs:/nfs",
		"-v", tools_dir + ":/vagrant/tools",
		"-d", "--tmpfs", "/tmp",
		"--ulimit", "nofile=262144:262144",
		"--network", anydbver_common.MakeContainerHostName(logger, namespace, "anydbver"),
		"--hostname", name}

	if mem, ok := args["memory"]; ok {
		cmd_args = append(cmd_args, "--memory="+mem)
	}

	user := "default"
	if u, ok := args["user"]; ok && u != "" {
		user = u
	}
	password := anydbver_common.ANYDBVER_DEFAULT_PASSWORD
	if pass, ok := args["password"]; ok {
		password = pass
	}
	cluster := "cluster1"
	if c, ok := args["cluster"]; ok && c != "" {
		cluster = c
	}
	shard := "1"
	if s, ok := args["shard"]; ok && s != "" {
		shard = s
	}

	// The same layout the ansible role builds from the inventory: members
	// share a Keeper root, the root and the next two members run Keeper when
	// there are three or more, and remote_servers is a static list with
	// internal_replication on.
	root := name
	if master, ok := args["master"]; ok && master != "" {
		root = master
	}
	members := clickHouseMembers(root, name, shard)
	keepers := []ClickHouseNode{}
	for _, m := range members {
		if m.Name == root {
			keepers = append(keepers, m)
		}
	}
	if len(members) >= 3 {
		for _, m := range members {
			if m.Name != root && len(keepers) < 3 {
				keepers = append(keepers, m)
			}
		}
	}
	host := func(node string) string { return anydbver_common.MakeContainerHostName(logger, namespace, node) }
	esc := html.EscapeString

	keeper_server := ""
	zookeeper := ""
	raft := ""
	for i, k := range keepers {
		raft += fmt.Sprintf("      <server><id>%d</id><hostname>%s</hostname><port>9234</port></server>\n", i+1, host(k.Name))
		zookeeper += fmt.Sprintf("    <node><host>%s</host><port>9181</port></node>\n", host(k.Name))
	}
	for i, k := range keepers {
		if k.Name == name {
			keeper_server = fmt.Sprintf(`  <keeper_server>
    <tcp_port>9181</tcp_port>
    <server_id>%d</server_id>
    <log_storage_path>/var/lib/clickhouse/coordination/log</log_storage_path>
    <snapshot_storage_path>/var/lib/clickhouse/coordination/snapshots</snapshot_storage_path>
    <raft_configuration>
%s    </raft_configuration>
  </keeper_server>
`, i+1, raft)
		}
	}

	shards := map[int][]string{}
	shardOrder := []int{}
	for _, m := range members {
		if _, ok := shards[m.Shard]; !ok {
			shardOrder = append(shardOrder, m.Shard)
		}
		shards[m.Shard] = append(shards[m.Shard], host(m.Name))
	}
	sort.Ints(shardOrder)
	remote := ""
	for _, sh := range shardOrder {
		remote += "      <shard>\n        <internal_replication>true</internal_replication>\n"
		for _, h := range shards[sh] {
			remote += fmt.Sprintf("        <replica><host>%s</host><port>9000</port><user>%s</user><password>%s</password></replica>\n", h, esc(user), esc(password))
		}
		remote += "      </shard>\n"
	}

	// The image's own config.d already listens on all addresses.
	config := fmt.Sprintf(`<clickhouse>
  <interserver_http_host>%s</interserver_http_host>
  <macros>
    <cluster>%s</cluster>
    <shard>%s</shard>
    <replica>%s</replica>
  </macros>
%s  <zookeeper>
%s  </zookeeper>
  <remote_servers>
    <%s>
%s    </%s>
  </remote_servers>
</clickhouse>`, self, esc(cluster), esc(shard), name, keeper_server, zookeeper,
		esc(cluster), remote, esc(cluster))

	client := fmt.Sprintf(`<config>
  <user>%s</user>
  <password>%s</password>
</config>`, esc(user), esc(password))

	// The image entrypoint creates the user from CLICKHOUSE_USER and
	// CLICKHOUSE_PASSWORD, then drops privileges and starts the server.
	entrypoint := fmt.Sprintf(`mkdir -p /etc/clickhouse-server/config.d /root/.clickhouse-client
cat > /etc/clickhouse-server/config.d/anydbver.xml <<'ANYDBVER_EOF'
%s
ANYDBVER_EOF
cat > /root/.clickhouse-client/config.xml <<'ANYDBVER_EOF'
%s
ANYDBVER_EOF
exec /entrypoint.sh
`, config, client)

	cmd_args = append(cmd_args,
		"-e", "CLICKHOUSE_USER="+user,
		"-e", "CLICKHOUSE_PASSWORD="+password,
		"-e", "CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1",
		"--entrypoint=/bin/sh")

	version := args["version"]
	if version == "" {
		version = "latest"
	}

	cmd_args = anydbver_common.AppendCPUParams(cmd_args, args)
	cmd_args = anydbver_common.AppendExposeParams(cmd_args, args)
	cmd_args = append(cmd_args, args["docker-image"]+":"+version, "-c", entrypoint)

	env := map[string]string{}
	errMsg := "Error creating container"
	ignoreMsg := regexp.MustCompile("ignore this")
	runtools.RunFatal(logger, cmd_args, errMsg, ignoreMsg, true, env)
}

// WaitClickHouse waits until every docker-image ClickHouse node accepts
// queries. It runs after all of them exist: a Keeper member does not start
// serving until its Raft group has a quorum, so waiting right after each
// docker run held node0 for two minutes while node1 and node2 did not exist
// yet. docker run returns before the server listens, so without the wait the
// first clickhouse-client after the deploy could fail.
func WaitClickHouse(logger *log.Logger, namespace string) {
	for _, n := range clickHouseNodes {
		self := anydbver_common.MakeContainerHostName(logger, namespace, n.Name)
		ready := false
		for i := 0; i < 60 && !ready; i++ {
			if exec.Command("docker", "exec", self, "clickhouse-client", "-q", "select 1").Run() == nil {
				ready = true
			} else {
				time.Sleep(2 * time.Second)
			}
		}
		if !ready {
			logger.Printf("Warning: ClickHouse in %s did not accept queries within 2 minutes\n", self)
		}
	}
}
