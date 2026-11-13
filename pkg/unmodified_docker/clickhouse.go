package unmodifieddocker

import (
	"fmt"
	"html"
	"log"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

// CreateClickHouseContainer starts the official clickhouse/clickhouse-server
// image. Every node runs a ClickHouse Keeper only when it has no master, and
// every node joins the same auto-discovered cluster, so "master=node0" is all
// a replica needs. Nodes find each other by container name: the --hostname is
// not resolvable from the other containers.
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

	keeper_host := self
	keeper_server := ""
	if master, ok := args["master"]; ok && master != "" {
		keeper_host = anydbver_common.MakeContainerHostName(logger, namespace, master)
	} else {
		keeper_server = fmt.Sprintf(`  <keeper_server>
    <tcp_port>9181</tcp_port>
    <server_id>1</server_id>
    <log_storage_path>/var/lib/clickhouse/coordination/log</log_storage_path>
    <snapshot_storage_path>/var/lib/clickhouse/coordination/snapshots</snapshot_storage_path>
    <raft_configuration>
      <server><id>1</id><hostname>%s</hostname><port>9234</port></server>
    </raft_configuration>
  </keeper_server>
`, self)
	}

	esc := html.EscapeString
	// The image's own config.d already listens on all addresses.
	config := fmt.Sprintf(`<clickhouse>
  <interserver_http_host>%s</interserver_http_host>
  <macros>
    <cluster>%s</cluster>
    <shard>%s</shard>
    <replica>%s</replica>
  </macros>
%s  <zookeeper>
    <node><host>%s</host><port>9181</port></node>
  </zookeeper>
  <allow_experimental_cluster_discovery>1</allow_experimental_cluster_discovery>
  <remote_servers>
    <%s>
      <discovery>
        <path>/clickhouse/discovery/%s</path>
        <my_hostname>%s</my_hostname>
        <shard>%s</shard>
        <user>%s</user>
        <password>%s</password>
      </discovery>
    </%s>
  </remote_servers>
</clickhouse>`, self, esc(cluster), esc(shard), name, keeper_server, keeper_host,
		esc(cluster), esc(cluster), self, esc(shard), esc(user), esc(password), esc(cluster))

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

	// docker run returns before the server listens. Wait, so the first
	// clickhouse-client after the deploy works.
	for i := 0; i < 60; i++ {
		if err := exec.Command("docker", "exec", self, "clickhouse-client", "-q", "select 1").Run(); err == nil {
			return
		}
		time.Sleep(2 * time.Second)
	}
	logger.Printf("Warning: ClickHouse in %s did not accept queries within 2 minutes\n", self)
}
