package unmodifieddocker

import (
	"log"
	"regexp"
	"strings"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

// Coroot stores metrics in Prometheus and logs/traces/profiles in ClickHouse,
// so a coroot node is really a small stack. The versions here are the ones the
// upstream deploy/docker-compose.yaml pins.
const (
	COROOT_PROMETHEUS_IMAGE    = "prom/prometheus:v2.53.5"
	COROOT_CLICKHOUSE_IMAGE    = "clickhouse/clickhouse-server:24.3"
	COROOT_NODE_AGENT_IMAGE    = "ghcr.io/coroot/coroot-node-agent"
	COROOT_CLUSTER_AGENT_IMAGE = "ghcr.io/coroot/coroot-cluster-agent"
	COROOT_UI_PORT             = "8080"
	// Coroot's UI login. Deliberately not ANYDBVER_DEFAULT_PASSWORD: this is a
	// throwaway local UI, and "admin" matches what k8s-pmm-ha already uses.
	COROOT_ADMIN_USER     = "admin"
	COROOT_ADMIN_PASSWORD = "admin"
	// Small image used only to write /etc/machine-id on the docker host.
	COROOT_HOSTFIX_IMAGE = "alpine:3"
)

func corootSidecar(logger *log.Logger, namespace string, name string, suffix string) string {
	return anydbver_common.MakeContainerHostName(logger, namespace, name+"-"+suffix)
}

// CreateCorootContainer brings up the coroot server plus the ClickHouse and
// Prometheus it needs, and the cluster-agent that scrapes databases.
//
// The node-agent is deliberately not started here. It only ever tracks
// containers that already existed when it started, so it has to come up after
// the database nodes are deployed. StartCorootNodeAgent does that from the
// post-deploy step.
func CreateCorootContainer(logger *log.Logger, namespace string, name string, cmd string, args map[string]string) {
	env := map[string]string{}
	errMsg := "Error creating coroot container"
	ignoreMsg := regexp.MustCompile("ignore this")

	network := anydbver_common.MakeContainerHostName(logger, namespace, "anydbver")
	clickhouse := corootSidecar(logger, namespace, name, "clickhouse")
	prometheus := corootSidecar(logger, namespace, name, "prometheus")
	clusterAgent := corootSidecar(logger, namespace, name, "cluster-agent")

	// This runs only when the coroot node itself is absent, so any sidecar
	// still carrying one of these names is a leftover of the node being
	// rebuilt. Without this the docker run below fails on the name.
	runtools.RunFatal(logger, append([]string{"docker", "rm", "-f", "-v"},
		clickhouse, prometheus, clusterAgent,
		corootSidecar(logger, namespace, name, "node-agent")),
		"Error removing leftover coroot sidecars", regexp.MustCompile("No such container|is not running"), false, env)

	version := args["version"]
	if version == "" {
		version = "latest"
	}

	runtools.RunFatal(logger, []string{
		"docker", "run", "--name", clickhouse, "-d",
		"--network", network, "--hostname", clickhouse,
		"-e", "CLICKHOUSE_SKIP_USER_SETUP=1",
		"--ulimit", "nofile=262144:262144",
		COROOT_CLICKHOUSE_IMAGE,
	}, errMsg, ignoreMsg, true, env)

	runtools.RunFatal(logger, []string{
		"docker", "run", "--name", prometheus, "-d",
		"--network", network, "--hostname", prometheus,
		COROOT_PROMETHEUS_IMAGE,
		"--config.file=/etc/prometheus/prometheus.yml",
		"--storage.tsdb.path=/prometheus",
		"--web.enable-lifecycle",
		"--web.enable-remote-write-receiver",
	}, errMsg, ignoreMsg, true, env)

	cmd_args := []string{
		"docker", "run", "--name", anydbver_common.MakeContainerHostName(logger, namespace, name), "-d",
		"--network", network,
		"--hostname", anydbver_common.MakeContainerHostName(logger, namespace, name),
		"-u", "root",
		"-e", "AUTH_BOOTSTRAP_ADMIN_PASSWORD=" + COROOT_ADMIN_PASSWORD,
	}
	if mem, ok := args["memory"]; ok {
		cmd_args = append(cmd_args, "--memory="+mem)
	}
	if _, ok := args["expose"]; ok {
		cmd_args = anydbver_common.AppendExposeParams(cmd_args, args)
	} else if port, ok := args["port"]; ok {
		cmd_args = append(cmd_args, "-p", port+":"+COROOT_UI_PORT)
	} else {
		cmd_args = append(cmd_args, "-p", ":"+COROOT_UI_PORT)
	}
	cmd_args = append(cmd_args,
		args["docker-image"]+":"+version,
		"--data-dir=/data",
		"--bootstrap-prometheus-url=http://"+prometheus+":9090",
		"--bootstrap-refresh-interval=15s",
		"--bootstrap-clickhouse-address="+clickhouse+":9000",
	)
	runtools.RunFatal(logger, cmd_args, errMsg, ignoreMsg, true, env)

	runtools.RunFatal(logger, []string{
		"docker", "run", "--name", clusterAgent, "-d",
		"--network", network, "--hostname", clusterAgent,
		COROOT_CLUSTER_AGENT_IMAGE,
		"--coroot-url=http://" + anydbver_common.MakeContainerHostName(logger, namespace, name) + ":" + COROOT_UI_PORT,
		"--metrics-scrape-interval=15s",
		"--metrics-wal-dir=/tmp",
	}, errMsg, ignoreMsg, true, env)
}

// ensureHostMachineID makes sure the docker host has a machine-id the
// node-agent can read. The agent reads it through /proc/1/root and reports it
// as a metric label that coroot uses to group containers into nodes; without it
// the node views stay empty.
//
// Real Linux hosts always have one, so this is a no-op there. The Docker
// Desktop VM does not, and its /etc/machine-id is a symlink to
// /var/lib/machine-id: an absolute symlink read through /proc/1/root resolves
// against the reading container's root, not the host's, so the agent cannot
// follow it. Hence writing the real files rather than the symlink, and
// /var/lib/dbus/machine-id, which is the next path the agent tries.
func ensureHostMachineID(logger *log.Logger) {
	env := map[string]string{}
	ignoreMsg := regexp.MustCompile("ignore this")
	script := `[ -s /proc/1/root/var/lib/dbus/machine-id ] && exit 0
id=$(cat /proc/1/root/var/lib/machine-id 2>/dev/null)
if [ -z "$id" ]; then
  id=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
  echo "$id" > /proc/1/root/var/lib/machine-id
fi
mkdir -p /proc/1/root/var/lib/dbus
echo "$id" > /proc/1/root/var/lib/dbus/machine-id
echo "created a machine-id on the docker host so coroot can identify it"`
	out, err := runtools.RunGetOutput(logger, []string{
		"docker", "run", "--rm", "--privileged", "--pid", "host",
		COROOT_HOSTFIX_IMAGE, "sh", "-c", script,
	}, "Could not check the docker host machine-id", ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	if err != nil {
		logger.Printf("Warning: could not ensure /etc/machine-id on the docker host: %v", err)
		return
	}
	if strings.TrimSpace(out) != "" {
		logger.Println(strings.TrimSpace(out))
	}
}

// StartCorootNodeAgent starts the node-agent for a coroot server node, or
// restarts it if it is already running.
//
// The restart is not optional. The agent builds its container list by scanning
// /proc when it starts and never picks up containers created later, so every
// deploy that adds nodes needs the agent bounced afterwards. docker restart is
// not reliable for a --pid host container, stop and start are.
func StartCorootNodeAgent(logger *log.Logger, namespace string, serverNode string) {
	env := map[string]string{}
	errMsg := "Error starting coroot node-agent"
	ignoreMsg := regexp.MustCompile("No such container|is not running")

	ensureHostMachineID(logger)

	agent := corootSidecar(logger, namespace, serverNode, "node-agent")
	server := anydbver_common.MakeContainerHostName(logger, namespace, serverNode)
	network := anydbver_common.MakeContainerHostName(logger, namespace, "anydbver")
	prefix := anydbver_common.MakeContainerHostName(logger, namespace, "")

	running, _ := runtools.RunGetOutput(logger, []string{
		"docker", "ps", "-a", "--filter", "name=^/" + agent + "$", "--format", "{{.Names}}",
	}, "Error listing coroot node-agent", ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)

	if strings.TrimSpace(running) == agent {
		runtools.RunFatal(logger, []string{"docker", "stop", agent}, errMsg, ignoreMsg, true, env)
		runtools.RunFatal(logger, []string{"docker", "start", agent}, errMsg, ignoreMsg, true, env)
		return
	}

	runtools.RunFatal(logger, []string{
		"docker", "run", "--name", agent, "-d",
		"--network", network, "--hostname", agent,
		"--privileged", "--pid", "host", "--init",
		"-v", "/sys/kernel/tracing:/sys/kernel/tracing",
		"-v", "/sys/kernel/debug:/sys/kernel/debug",
		"-v", "/sys/fs/cgroup:/host/sys/fs/cgroup",
		COROOT_NODE_AGENT_IMAGE,
		"--collector-endpoint=http://" + server + ":" + COROOT_UI_PORT,
		"--cgroupfs-root=/host/sys/fs/cgroup",
		"--wal-dir=/tmp",
		// Keep one namespace's coroot from reporting on every other container
		// on the machine: --pid host means it can see all of them.
		"--container-allowlist=^/docker/" + regexp.QuoteMeta(prefix),
	}, errMsg, ignoreMsg, true, env)
}
