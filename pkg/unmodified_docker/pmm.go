package unmodifieddocker

import (
	"fmt"
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

func CreatePMMContainer(logger *log.Logger, namespace string, name string, cmd string, args map[string]string) {
	cmd_args := []string{
		"docker", "run",
		"--name", anydbver_common.MakeContainerHostName(logger, namespace, name),
		"-d",
		"--network", anydbver_common.MakeContainerHostName(logger, namespace, "anydbver"),
		"--hostname", anydbver_common.MakeContainerHostName(logger, namespace, name),
	}
	if mem, ok := args["memory"]; ok {
		cmd_args = append(cmd_args, "--memory="+mem)
	}
	cmd_args = anydbver_common.AppendCPUParams(cmd_args, args)

	pmmPort := "8443"
	if strings.HasPrefix(args["version"], "1.") || strings.HasPrefix(args["version"], "2.") {
		pmmPort = "443"
	}

	if _, ok := args["expose"]; ok {
		cmd_args = anydbver_common.AppendExposeParams(cmd_args, args)
	} else if port, ok := args["port"]; ok {
		cmd_args = append(cmd_args, "-p", port+":"+pmmPort)
	} else {
		cmd_args = append(cmd_args, "-p", ":"+pmmPort)
	}

	cmd_args = append(cmd_args, args["docker-image"]+":"+args["version"])

	env := map[string]string{}
	errMsg := "Error creating container"
	ignoreMsg := regexp.MustCompile("ignore this")
	runtools.RunFatal(logger, cmd_args, errMsg, ignoreMsg, true, env)

	runtools.RunFatal(logger, []string{
		"docker", "exec", anydbver_common.MakeContainerHostName(logger, namespace, name), "bash", "-c",
		// Retry until Grafana's database is up. A fixed 30s wait failed as soon
		// as the container ran with a CPU limit and started slower.
		"for i in $(seq 60); do sleep 5; grafana-cli --config /etc/grafana/grafana.ini --homepath /usr/share/grafana --configOverrides cfg:default.paths.data=/srv/grafana admin reset-admin-password " + anydbver_common.ANYDBVER_DEFAULT_PASSWORD + " && exit 0; done; exit 1",
	}, "Error changing password", ignoreMsg, true, env)

	// nginx "worker_processes auto" starts one worker per host core and sees
	// through both --cpus and --cpuset-cpus: 48 workers on a 48 core host.
	workers := nginxWorkers(args)
	runtools.RunFatal(logger, []string{
		"docker", "exec", anydbver_common.MakeContainerHostName(logger, namespace, name), "bash", "-c",
		fmt.Sprintf("sed -i 's/^worker_processes auto;/worker_processes %d;/' /etc/nginx/nginx.conf && nginx -s reload", workers),
	}, "Error limiting nginx workers", ignoreMsg, true, env)
}

// nginxWorkers is one worker per CPU the container may use, 2 when unlimited.
func nginxWorkers(args map[string]string) int {
	if cpus, err := strconv.ParseFloat(args["cpus"], 64); err == nil && cpus > 0 {
		return int(math.Ceil(cpus))
	}
	if cpuset := args["cpuset"]; cpuset != "" {
		count := 0
		for _, part := range strings.Split(cpuset, ",") {
			bounds := strings.SplitN(part, "-", 2)
			first, err1 := strconv.Atoi(bounds[0])
			last, err2 := strconv.Atoi(bounds[len(bounds)-1])
			if err1 == nil && err2 == nil && last >= first {
				count += last - first + 1
			}
		}
		if count > 0 {
			return count
		}
	}
	return 2
}
