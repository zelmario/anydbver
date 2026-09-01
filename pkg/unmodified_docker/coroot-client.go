package unmodifieddocker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

// CorootTarget is one database anydbver registers with a coroot server.
type CorootTarget struct {
	Server   string // coroot server node, e.g. "node0"
	Node     string // database node, e.g. "node1"
	Type     string // coroot instrumentation type: postgres, mysql, mongodb, redis
	Port     string
	User     string
	Password string
}

// CorootInstrumentationType maps an anydbver database keyword to the coroot
// instrumentation type that scrapes it, and to that database's default port.
// An empty type means the keyword is not a database coroot can scrape.
func CorootInstrumentationType(cmd string) (string, string) {
	switch cmd {
	case "percona-server-mongodb", "mongodb":
		return "mongodb", "27017"
	case "postgresql", "percona-postgresql":
		return "postgres", "5432"
	case "percona-server", "mysql", "mariadb", "percona-xtradb-cluster":
		return "mysql", "3306"
	case "valkey", "redis":
		return "redis", "6379"
	}
	return "", ""
}

type corootApi struct {
	base   string
	client *http.Client
	logger *log.Logger
}

func newCorootApi(logger *log.Logger, base string) (*corootApi, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	api := &corootApi{
		base:   base,
		client: &http.Client{Jar: jar, Timeout: 30 * time.Second},
		logger: logger,
	}
	body, _ := json.Marshal(map[string]string{
		"email":    COROOT_ADMIN_USER,
		"password": COROOT_ADMIN_PASSWORD,
	})
	resp, err := api.client.Post(base+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("coroot login returned %d", resp.StatusCode)
	}
	return api, nil
}

// projectId resolves coroot's generated project id. Only the project name is
// stable ("default" for a fresh install), the id is random per install.
func (a *corootApi) projectId() (string, error) {
	resp, err := a.client.Get(a.base + "/api/user")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var user struct {
		Projects []struct {
			Id   string `json:"id"`
			Name string `json:"name"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", err
	}
	if len(user.Projects) == 0 {
		return "", fmt.Errorf("coroot has no projects yet")
	}
	for _, p := range user.Projects {
		if p.Name == "default" {
			return p.Id, nil
		}
	}
	return user.Projects[0].Id, nil
}

// appId waits for coroot to model the container as an application. Nothing can
// be configured on an application coroot has not seen yet, and it only appears
// after the node-agent has reported it and coroot has rebuilt its world.
//
// A plain docker container becomes "<project>:_:Unknown:<container>", so probe
// that id directly: the instrumentation endpoint answers 200 for an existing
// application and 404 otherwise. The overview list is only a fallback, it does
// not always carry an application that already exists.
func (a *corootApi) appId(project string, container string, kind string, timeout time.Duration) (string, error) {
	guess := project + ":_:Unknown:" + container
	started := time.Now()
	deadline := started.Add(timeout)
	reported := time.Now()
	for {
		if a.appExists(project, guess, kind) {
			return guess, nil
		}
		if id, ok := a.findApp(project, container); ok {
			return id, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("coroot did not discover %s within %s", container, timeout)
		}
		// This wait is a minute or two of nothing, so say what it is waiting on.
		if time.Since(reported) >= 20*time.Second {
			a.logger.Printf("Coroot: waiting for %s to appear in coroot (%ds elapsed, up to %s)",
				container, int(time.Since(started).Seconds()), timeout)
			reported = time.Now()
		}
		time.Sleep(5 * time.Second)
	}
}

func (a *corootApi) appExists(project string, app string, kind string) bool {
	resp, err := a.client.Get(a.base + "/api/project/" + project + "/app/" + url.PathEscape(app) + "/instrumentation/" + kind)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (a *corootApi) findApp(project string, container string) (string, bool) {
	resp, err := a.client.Get(a.base + "/api/project/" + project + "/overview/applications")
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	var overview struct {
		Data struct {
			Applications []struct {
				Id string `json:"id"`
			} `json:"applications"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&overview); err != nil {
		return "", false
	}
	for _, app := range overview.Data.Applications {
		if strings.HasSuffix(app.Id, ":"+container) {
			return app.Id, true
		}
	}
	return "", false
}

func (a *corootApi) instrument(project string, app string, t CorootTarget) error {
	body, _ := json.Marshal(map[string]any{
		"type": t.Type,
		"port": t.Port,
		"credentials": map[string]string{
			"username": t.User,
			"password": t.Password,
		},
		"enabled": true,
	})
	u := a.base + "/api/project/" + project + "/app/" + url.PathEscape(app) + "/instrumentation/" + t.Type
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("coroot returned %d for %s", resp.StatusCode, t.Node)
	}
	return nil
}

// corootServerURL returns the host-side URL of a coroot server node.
func corootServerURL(logger *log.Logger, namespace string, serverNode string) (string, error) {
	env := map[string]string{}
	ignoreMsg := regexp.MustCompile("ignore this")
	out, err := runtools.RunGetOutput(logger, []string{
		"docker", "port", anydbver_common.MakeContainerHostName(logger, namespace, serverNode), COROOT_UI_PORT + "/tcp",
	}, "Error getting the coroot port", ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if idx := strings.LastIndex(line, ":"); idx != -1 {
			port := strings.TrimSpace(line[idx+1:])
			if port != "" {
				return "http://127.0.0.1:" + port, nil
			}
		}
	}
	return "", fmt.Errorf("coroot node %s does not publish port %s", serverNode, COROOT_UI_PORT)
}

// waitForClusterAgentScrape blocks until the cluster-agent has actually
// collected from a database, or the timeout passes. It polls the agent's own
// log, which reports every completed collection.
func waitForClusterAgentScrape(logger *log.Logger, namespace string, server string, timeout time.Duration) bool {
	agent := corootSidecar(logger, namespace, server, "cluster-agent")
	env := map[string]string{}
	ignoreMsg := regexp.MustCompile("ignore this")
	deadline := time.Now().Add(timeout)
	for {
		out, err := runtools.RunGetOutput(logger, []string{
			"docker", "logs", "--since", "5m", agent,
		}, "Error reading the cluster-agent log", ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
		if err == nil && strings.Contains(out, "metrics collection completed") {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Second)
	}
}

// SetupCoroot starts the node-agent for every coroot server in the deploy and
// registers every coroot-client database with its server.
//
// It runs after the playbook, when the database nodes are up. The node-agent
// has to be (re)started with them already running, and coroot can only be told
// about an application it has already discovered.
func SetupCoroot(logger *log.Logger, namespace string, servers []string, targets []CorootTarget) {
	for _, t := range targets {
		if !contains(servers, t.Server) {
			servers = append(servers, t.Server)
		}
	}
	if len(servers) == 0 {
		return
	}

	for _, server := range servers {
		logger.Printf("Coroot: starting the node-agent on %s so it picks up the deployed nodes", server)
		StartCorootNodeAgent(logger, namespace, server)

		base, err := corootServerURL(logger, namespace, server)
		if err != nil {
			logger.Printf("Coroot: %v", err)
			continue
		}
		api, err := newCorootApi(logger, base)
		if err != nil {
			logger.Printf("Coroot: could not log in to %s: %v", base, err)
			continue
		}
		project, err := api.projectId()
		if err != nil {
			logger.Printf("Coroot: %v", err)
			continue
		}

		monitored := []string{}
		for _, t := range targets {
			if t.Server != server {
				continue
			}
			container := anydbver_common.MakeContainerHostName(logger, namespace, t.Node)
			app, err := api.appId(project, container, t.Type, 3*time.Minute)
			if err != nil {
				logger.Printf("Coroot: %v", err)
				continue
			}
			if t.Type == "postgres" {
				enablePgStatStatements(logger, namespace, t.Node)
			}
			if err := api.instrument(project, app, t); err != nil {
				logger.Printf("Coroot: could not enable %s monitoring for %s: %v", t.Type, t.Node, err)
				continue
			}
			logger.Printf("Coroot: %s monitoring enabled for %s", t.Type, t.Node)
			// The colons in an application id have to be percent-encoded for the
			// UI router, or coroot answers "invalid application id".
			monitored = append(monitored, fmt.Sprintf("  %-6s %-8s %s/p/%s/applications/%s",
				t.Node, t.Type, base, project, url.QueryEscape(app)))
		}

		// Coroot hides an application it has seen no traffic to or from, and
		// the node-agent records a connection only if it exists when the agent
		// scans. At the point the node-agent was started above, the
		// cluster-agent had not been told about these databases yet, so it was
		// not talking to them and no link was recorded. One more restart, once
		// it is actually scraping, is what puts the databases on the overview.
		if len(monitored) > 0 {
			logger.Println("Coroot: waiting for the cluster-agent to reach the databases")
			if !waitForClusterAgentScrape(logger, namespace, server, 3*time.Minute) {
				logger.Println("Coroot: the cluster-agent has not scraped yet, the databases may take a while to appear")
			}
			logger.Println("Coroot: restarting the node-agent so coroot links the databases to their clients")
			StartCorootNodeAgent(logger, namespace, server)
		}

		fmt.Println("")
		fmt.Println("Coroot UI:", base)
		fmt.Printf("Username: %s\n", COROOT_ADMIN_USER)
		fmt.Printf("Password: %s\n", COROOT_ADMIN_PASSWORD)
		if len(monitored) > 0 {
			fmt.Println("")
			fmt.Println("Monitored databases:")
			for _, line := range monitored {
				fmt.Println(line)
			}
			// Coroot's overview page lists an application only once it has seen
			// traffic to or from another application. A database nothing has
			// talked to yet is hidden there, so hand over direct links.
			fmt.Println("")
			fmt.Println("They take a couple of minutes to show up in coroot's application")
			fmt.Println("list. The links above work straight away.")
		}
		fmt.Println("")
	}
}

func contains(items []string, item string) bool {
	for _, v := range items {
		if v == item {
			return true
		}
	}
	return false
}

// pgStatStatementsScript configures pg_stat_statements on a Postgres node.
// Postgres only exposes per-query statistics through that extension, and the
// extension needs its library preloaded, which needs a restart. anydbver
// already does this for PMM in tools/setup_pmm.sh; coroot needs the same, done
// from here so the feature stays binary-only with no ansible role change.
//
// It preserves whatever is already preloaded (repmgr, pg_stat_monitor) instead
// of overwriting the list, and skips standbys, where ALTER SYSTEM is pointless
// and CREATE EXTENSION fails on a read-only server.
const pgStatStatementsScript = `
set -e
command -v systemctl >/dev/null 2>&1 || { echo "coroot: no systemd, skipping pg_stat_statements"; exit 0; }
PSQL="su - postgres -c"
recovery=$($PSQL "psql -tAc \"select pg_is_in_recovery()\"" 2>/dev/null | tr -d ' ')
if [ "$recovery" != "f" ]; then echo "coroot: standby, skipping pg_stat_statements"; exit 0; fi
current=$($PSQL "psql -tAc \"show shared_preload_libraries\"" | tr -d ' ')
case ",$current," in
  *,pg_stat_statements,*)
    ;;
  *)
    if [ -z "$current" ]; then new=pg_stat_statements; else new="$current,pg_stat_statements"; fi
    $PSQL "psql -c \"alter system set shared_preload_libraries to '$new'\""
    unit=$(systemctl list-units --plain --no-legend 'postgresql*' | awk '{print $1}' | head -1)
    if [ -n "$unit" ]; then
      systemctl restart "$unit"
      until $PSQL "psql -tAc \"select 1\"" >/dev/null 2>&1; do sleep 1; done
    fi
    ;;
esac
$PSQL "psql -c \"create extension if not exists pg_stat_statements\"" >/dev/null
echo "coroot: pg_stat_statements ready"
`

// enablePgStatStatements gives coroot per-query statistics on a Postgres node.
// Best effort: a failure here costs the query views, not the rest of the
// metrics, so it is logged and the deploy continues.
func enablePgStatStatements(logger *log.Logger, namespace string, node string) {
	container := anydbver_common.MakeContainerHostName(logger, namespace, node)
	env := map[string]string{}
	ignoreMsg := regexp.MustCompile("ignore this")
	out, err := runtools.RunGetOutput(logger, []string{
		"docker", "exec", container, "bash", "-c", pgStatStatementsScript,
	}, "Could not enable pg_stat_statements", ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	if err != nil {
		logger.Printf("Coroot: could not enable pg_stat_statements on %s, query statistics will be missing: %v", node, err)
		return
	}
	if line := strings.TrimSpace(out); line != "" {
		logger.Printf("Coroot: %s: %s", node, lastLine(line))
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
