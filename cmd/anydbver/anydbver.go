package main

// build with:  go build -ldflags=-X\ main.Version=$(git log --no-walk --tags --pretty="%H %d" --decorate=short | head -n1 | awk  -F'[, )]' '{ print $4; }')\ -X\ main.GoVersion=$(go version | cut -d " " -f3)\ -X\ main.Commit=$(git rev-list -1 HEAD)\ -X\ main.Build=$(date +%FT%T%z) -o tools/anydbver cmd/anydbver/anydbver.go

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/spf13/cobra"
	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
	unmodified_docker "github.com/zelmario/anydbver/pkg/unmodified_docker"
	versionfetch "github.com/zelmario/anydbver/pkg/version_fetch"
	"golang.org/x/term"
	_ "modernc.org/sqlite"
)

//go:embed Dockerfile.anydbver.cache
var embeddedDockerfileCache string

var (
	Build     = "unknown"
	GoVersion = "unknown"
	Version   = "unknown"
	Commit    = "unknown"
	// goreleaser
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

type ContainerConfig struct {
	Name       string
	OSVersion  string
	Privileged bool
	ExposePort string
	Provider   string
	Namespace  string
	Memory     string
	CPUs       string
	DeployArgs []string // All deployment keywords (e.g., ["postgresql:14"]) for cache image
	Keep       bool     // deploy --keep: reuse the container if it already exists
}

func getNetworkName(logger *log.Logger, namespace string) string {
	return anydbver_common.MakeContainerHostName(logger, namespace, "anydbver")
}

func listContainers(logger *log.Logger, provider string, namespace string) {
	if provider == "docker" {
		args := []string{"docker", "ps", "-a", "--filter", "network=" + getNetworkName(logger, namespace)}

		env := map[string]string{}
		errMsg := "Error docker ps"
		ignoreMsg := regexp.MustCompile("ignore this")

		containers, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
		if err != nil {
			logger.Printf("Can't list anydbver containers: %v", err)
			runtools.HandleDockerProblem(logger, err)
		}

		fmt.Print(containers)
	}
}

func getNsFromString(logger *log.Logger, input string) string {
	res := ""
	lines := strings.Split(input, "\n")

	suffix := getNetworkName(logger, "")

	for _, line := range lines {
		if strings.HasSuffix(line, suffix) {
			result := strings.TrimSuffix(line, suffix)
			if result == "" {
				res = res + "default\n"
			} else {
				result := strings.TrimSuffix(line, "-"+suffix)
				res = res + result + "\n"
			}
		}
	}
	return res
}

func getContainerIp(provider string, logger *log.Logger, namespace string, containerName string) (string, error) {
	network := getNetworkName(logger, namespace)
	if provider == "docker" {
		args := []string{"docker", "inspect", containerName, "--format", "{{ index .NetworkSettings.Networks \"" + network + "\" \"IPAddress\" }}"}

		env := map[string]string{}
		errMsg := "Error getting docker container ip"
		ignoreMsg := regexp.MustCompile("ignore this")

		ip, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
		return strings.TrimSuffix(ip, "\n"), err
	}
	return "", errors.New("node ip is not found")
}

func getContainerPort(logger *log.Logger, namespace string, name string) string {
	containerName := anydbver_common.MakeContainerHostName(logger, namespace, name)
	args := []string{"docker", "inspect", containerName, "--format", `{{range $p, $conf := .NetworkSettings.Ports}}{{$p}} {{end}}`}

	env := map[string]string{}
	errMsg := "Error getting docker container port"
	ignoreMsg := regexp.MustCompile("ignore this")

	output, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	if err != nil {
		return ""
	}
	output = strings.TrimSpace(output)
	if strings.Contains(output, "8443") {
		return ":8443"
	}
	if strings.Contains(output, "443") {
		return ":443"
	}
	return ""
}

func getNodeIp(provider string, logger *log.Logger, namespace string, name string) (string, error) {
	if provider == "docker" || provider == "docker-image" {
		return getContainerIp(provider, logger, namespace, anydbver_common.MakeContainerHostName(logger, namespace, name))
	}
	return "", errors.New("node ip is not found")
}

func listNamespaces(provider string, logger *log.Logger) {
	if provider == "docker" {
		args := []string{"docker", "network", "ls", "--format={{.Name}}"}

		env := map[string]string{}
		errMsg := "Error docker network"
		ignoreMsg := regexp.MustCompile("ignore this")

		networks, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
		if err != nil {
			logger.Printf("Can't list anydbver namespaces: %v", err)
			runtools.HandleDockerProblem(logger, err)
		}

		fmt.Print(getNsFromString(logger, networks))
	}
}

func findK3dClusters(logger *log.Logger, namespace string) []string {
	k3d_path, err := anydbver_common.GetK3dPath(logger)
	if k3d_path == "" || err != nil {
		return []string{}
	}

	net := getNetworkName(logger, namespace)
	args := []string{"docker", "ps", "-a", "--filter", "network=" + net, "--format", "{{.Names}}"}

	env := map[string]string{}
	errMsg := "Error docker ps"
	ignoreMsg := regexp.MustCompile("not found|No such")

	containers, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	if err != nil {
		logger.Printf("Can't list k3d clusters: %v", err)
		runtools.HandleDockerProblem(logger, err)
	}
	containers_list := slices.DeleteFunc(strings.Split(containers, "\n"), func(e string) bool {
		return e == ""
	})

	clusters := []string{}

	for _, name := range containers_list {
		if strings.HasSuffix(name, "-server-0") {
			clusters = append(clusters, strings.TrimPrefix(strings.TrimSuffix(name, "-server-0"), "k3d-"))
		}
	}

	return clusters
}

type AnydbverTest struct {
	id   int
	name string
	cmd  string
}

func FetchTests(logger *log.Logger, dbFile string, name string) ([]AnydbverTest, error) {
	var tests []AnydbverTest

	if name == "all" || name == "list" {
		name = "%"
	}

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return tests, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	query := `SELECT test_id, test_name, REPLACE(REPLACE(cmd,'./anydbver','anydbver'), 'default', 'node0') as cmd FROM tests WHERE test_name LIKE ? ORDER BY 1`

	rows, err := db.Query(query, name)
	if err != nil {
		return tests, fmt.Errorf("failed to execute select query: %w", err)
	}
	defer rows.Close()

	// Collect the results into a string
	for rows.Next() {
		var test AnydbverTest
		if err := rows.Scan(&test.id, &test.name, &test.cmd); err != nil {
			return tests, fmt.Errorf("failed to scan row: %w", err)
		}
		tests = append(tests, test)
	}
	if err = rows.Err(); err != nil {
		return tests, fmt.Errorf("error iterating over rows: %w", err)
	}

	return tests, nil
}

func FetchTestCases(logger *log.Logger, dbFile string, test_id int) ([]AnydbverTest, error) {
	var tests []AnydbverTest

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return tests, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	query := `SELECT test_id, REPLACE(REPLACE(REPLACE(cmd,'./anydbver','anydbver'), 'default', 'node0'), 'anydbver ssh', 'anydbver exec') as cmd FROM test_cases WHERE test_id = ? ORDER BY 1,2`

	rows, err := db.Query(query, test_id)
	if err != nil {
		return tests, fmt.Errorf("failed to execute select query: %w", err)
	}
	defer rows.Close()

	// Collect the results into a string
	for rows.Next() {
		var test AnydbverTest
		if err := rows.Scan(&test.id, &test.cmd); err != nil {
			return tests, fmt.Errorf("failed to scan row: %w", err)
		}
		tests = append(tests, test)
	}
	if err = rows.Err(); err != nil {
		return tests, fmt.Errorf("error iterating over rows: %w", err)
	}

	return tests, nil
}

func writeTestOutput(logger *log.Logger, test_id int, test_name string, test_output string) {
	test_results_path := filepath.Join(anydbver_common.GetCacheDirectory(logger), "test_results")
	err := os.MkdirAll(test_results_path, os.ModePerm)
	if err != nil {
		fmt.Printf("Failed to create directory: %s\n", err)
		return
	}
	file, err := os.Create(filepath.Join(test_results_path, fmt.Sprintf("%d - %s.log", test_id, test_name)))
	if err != nil {
		logger.Printf("Failed to create or open file: %s\n", err)
		return
	}

	defer func() {
		if err := file.Close(); err != nil {
			logger.Printf("Failed to close file: %s\n", err)
		}
	}()

	_, err = file.WriteString(test_output)
	if err != nil {
		logger.Printf("Failed to write to file: %s\n", err)
		return
	}
}

func testAnydbver(logger *log.Logger, _ string, _ string, name string, skip_os []string, registry_cache string) error {
	dbFile := anydbver_common.GetDatabasePath(logger)
	// Join the results with a space
	tests, err := FetchTests(logger, dbFile, name)
	if err != nil {
		return err
	}

outer:
	for _, test := range tests {
		if registry_cache != "" {
			re := regexp.MustCompile(`^(.*\s*k3d)(:\S+)?(\s.*|)$`)
			if strings.Contains(test.cmd, "k3d:") {
				test.cmd = re.ReplaceAllString(test.cmd, "$1$2,registry-cache="+registry_cache+"$3")
			} else {
				test.cmd = re.ReplaceAllString(test.cmd, "$1$2:latest,registry-cache="+registry_cache+"$3")
			}
		}
		logger.Printf("Test %+v", test)
		if name == "list" {
			continue
		}
		cmd_args := []string{
			"bash", "-c", test.cmd,
		}

		for _, os_name := range skip_os {
			if strings.Contains(test.name, os_name) || strings.Contains(test.cmd, "os:"+os_name) {
				logger.Println("SKIPPED")
				continue outer
			}
		}

		env := map[string]string{}
		errMsg := "Error running test"
		ignoreMsg := regexp.MustCompile("ignore this")
		out, err := runtools.RunGetOutput(logger, cmd_args, errMsg, ignoreMsg, true, env, runtools.COMMAND_TIMEOUT*2)
		if err != nil {
			logger.Println("FAILED")
			writeTestOutput(logger, test.id, test.name, out)
		} else {
			logger.Println("DEPLOYED")
			test_cases, err := FetchTestCases(logger, dbFile, test.id)
			if err != nil {
				return err
			}

			for test_case_no, test_case := range test_cases {
				logger.Printf("Test case: %s", test_case.cmd)
				cmd_args := []string{
					"bash", "-c", test_case.cmd,
				}

				errMsg := "Error running test"
				ignoreMsg := regexp.MustCompile("ignore this")
				out, err := runtools.RunGetOutput(logger, cmd_args, errMsg, ignoreMsg, true, env, runtools.COMMAND_TIMEOUT)
				if err != nil {
					logger.Println("test case FAILED")
					writeTestOutput(logger, test.id, test.name+" - "+fmt.Sprint(test_case_no), out)
				} else {
					logger.Println("PASSED")
				}

			}
		}

	}
	return nil
}

func removeCacheImages(logger *log.Logger) {
	user := strings.ReplaceAll(anydbver_common.GetUser(logger), ".", "-")
	cacheImagePattern := fmt.Sprintf("%s/anydbver-cache:*", user)

	// List all cache images
	args := []string{"docker", "images", "--format", "{{.Repository}}:{{.Tag}}", cacheImagePattern}
	env := map[string]string{}
	errMsg := "Error listing cache images"
	ignoreMsg := regexp.MustCompile("ignore this")

	output, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	if err != nil {
		logger.Printf("Warning: Could not list cache images: %v\n", err)
		return
	}

	images := slices.DeleteFunc(strings.Split(output, "\n"), func(e string) bool {
		return strings.TrimSpace(e) == ""
	})

	if len(images) == 0 {
		logger.Printf("No cache images found to remove\n")
		return
	}

	logger.Printf("Removing %d cache image(s)...\n", len(images))

	// Delete each cache image
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" {
			continue
		}
		deleteArgs := []string{"docker", "rmi", "-f", image}
		errMsg := fmt.Sprintf("Error removing cache image %s", image)
		ignoreMsg := regexp.MustCompile("No such image|image is being used|image is referenced")
		runtools.RunFatal(logger, deleteArgs, errMsg, ignoreMsg, true, env)
		logger.Printf("Removed cache image: %s\n", image)
	}
}

// unmountNfsInContainers lazy-unmounts every NFS mount inside each of the given
// containers. It reads /proc/mounts so it handles client mounts at any path
// (the default /mnt/nfs as well as a custom nfs-client:mount=/path). All errors
// are ignored: a container with no NFS mount, or one that is already gone, is a
// no-op.
func unmountNfsInContainers(logger *log.Logger, containers []string) {
	env := map[string]string{}
	errMsg := "Error unmounting NFS shares before destroy"
	// This step is best-effort: failing to unmount must never block the
	// destroy that follows. RunGetOutput returns the error instead of calling
	// Fatalf, so we discard it. A container with no NFS mount, one that is
	// already gone, or one that is wedged (exec fails with an OCI/setns error)
	// is all the same here — a no-op that hands control to docker rm -f.
	for _, container := range containers {
		args := []string{
			"docker", "exec", container, "sh", "-c",
			"awk '$3 ~ /^nfs/ {print $2}' /proc/mounts | xargs -r umount -l",
		}
		runtools.RunGetOutput(logger, args, errMsg, nil, false, env, runtools.COMMAND_TIMEOUT)
	}
}

// unpauseChaosContainers unpauses every paused container in the given list. The
// chaos feature (chaos pause / the chaos monkey) can leave a container paused; a
// paused container cannot receive the SIGKILL that docker rm -f sends, so destroy
// could wedge on it the same way a live NFS mount wedges the kernel. Unpausing
// first is best-effort: a container that is not paused (or already gone) makes
// docker unpause error harmlessly, and we discard that error so nothing blocks
// the destroy that follows.
func unpauseChaosContainers(logger *log.Logger, containers []string) {
	env := map[string]string{}
	errMsg := "Error unpausing container before destroy"
	ignoreMsg := regexp.MustCompile("is not paused|No such container|not running")
	for _, container := range containers {
		args := []string{"docker", "unpause", container}
		runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	}
}

func deleteNamespace(logger *log.Logger, provider string, namespace string) {
	if provider == "docker" {
		k3d_path, err := anydbver_common.GetK3dPath(logger)
		if k3d_path != "" {
			for _, cluster_name := range findK3dClusters(logger, namespace) {
				k3d_create_cmd := []string{k3d_path, "cluster", "delete", cluster_name}
				env := map[string]string{}
				errMsg := "Error deleting k3d cluster"
				ignoreMsg := regexp.MustCompile("No clusters found")
				runtools.RunFatal(logger, k3d_create_cmd, errMsg, ignoreMsg, true, env)
			}
		}

		net := getNetworkName(logger, namespace)
		args := []string{"docker", "ps", "-a", "--filter", "network=" + net, "--format", "{{.ID}}"}

		env := map[string]string{}
		errMsg := "Error docker ps"
		ignoreMsg := regexp.MustCompile("not found|No such|has active endpoints")

		containers, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
		if err != nil {
			logger.Fatalf("Can't list anydbver containers to delete: %v", err)
		}
		containers_list := slices.DeleteFunc(strings.Split(containers, "\n"), func(e string) bool {
			return e == ""
		})

		if len(containers_list) > 0 {

			// Lazy-unmount any NFS shares inside the containers before removing
			// them. While every container is still alive the NFS server is still
			// responsive, so the unmount succeeds. If the server container were
			// killed first, the client kernel would hang on the dead mount and
			// Docker would fail with "did not receive an exit event".
			unmountNfsInContainers(logger, containers_list)

			// Unpause any chaos-paused container so docker rm -f can deliver its
			// SIGKILL. Best-effort: tc netem shaping needs no cleanup here, it
			// dies with each container's network namespace on removal.
			unpauseChaosContainers(logger, containers_list)

			// Remove any chaos helper containers (they join a node's netns via
			// --net=container:, so they aren't on the namespace network and would
			// otherwise be orphaned when their node is removed).
			chaosCleanupHelpers(logger, "docker", namespace)

			delete_args := []string{"docker", "rm", "-f", "-v"}
			delete_args = append(delete_args, containers_list...)
			runtools.RunFatal(logger, delete_args, errMsg, ignoreMsg, true, env)
		}
		delete_args := []string{"docker", "network", "rm", net}
		runtools.RunFatal(logger, delete_args, errMsg, ignoreMsg, true, env)
		os.Remove(anydbver_common.GetAnsibleInventory(logger, namespace))
		// Drop the chaos link state so a later redeploy of this namespace can't
		// resurrect stale faults from a previous run.
		os.Remove(chaosStatePath(logger, namespace))

	}
}

func ConvertStringToMap(input string) map[string]string {
	result := make(map[string]string)
	pairs := strings.Split(input, ",")
	for _, pair := range pairs {
		keyValue := strings.Split(pair, "=")
		if len(keyValue) == 2 {
			key := keyValue[0]
			value := keyValue[1]
			result[key] = value
		}
	}
	return result
}

func createNamespace(logger *log.Logger, containers []ContainerConfig, namespace string) {
	network := getNetworkName(logger, namespace)
	netCreated := false
	for _, container := range containers {
		if netCreated == false && (container.Provider == "docker" || container.Provider == "docker-image" || container.Provider == "kubectl") {
			args := []string{"docker", "network", "create", network}
			env := map[string]string{}
			errMsg := "Error creating docker network"
			ignoreMsg := regexp.MustCompile("already exists")
			runtools.RunFatal(logger, args, errMsg, ignoreMsg, true, env)
			netCreated = true
		}

		if container.Provider != "docker-image" {
			createContainer(logger, container)
		}
	}
}

func computeSHA256(input string) string {
	hash := sha256.Sum256([]byte(input))
	return hex.EncodeToString(hash[:])
}

func getBaseImageForOS(osver string) string {
	// Map OS versions to their base Docker images
	baseImageMap := map[string]string{
		"el7":          "centos:centos7",
		"el8":          "rockylinux:8",
		"el9":          "rockylinux:9",
		"el10":         "rockylinux/rockylinux:10",
		"focal":        "ubuntu:focal",
		"20.04":        "ubuntu:focal",
		"ubuntu-20.04": "ubuntu:focal",
		"ubuntu20.04":  "ubuntu:focal",
		"jammy":        "ubuntu:jammy",
		"22.04":        "ubuntu:jammy",
		"ubuntu-22.04": "ubuntu:jammy",
		"ubuntu22.04":  "ubuntu:jammy",
		"noble":        "ubuntu:noble",
		"24.04":        "ubuntu:noble",
		"ubuntu-24.04": "ubuntu:noble",
		"ubuntu24.04":  "ubuntu:noble",
		"bookworm":     "debian:bookworm",
		"debian-12":    "debian:bookworm",
	}

	if baseImage, ok := baseImageMap[osver]; ok {
		return baseImage
	}
	// Default to rockylinux:8
	return "rockylinux:8"
}

func buildCacheImage(logger *log.Logger, deployArgs []string, osver string, user string) string {
	if len(deployArgs) == 0 {
		return ""
	}

	// Generate ansible deployment arguments from deployment keywords
	ansible_deployment_args := ""
	for _, arg := range deployArgs {
		deployment_keyword := ParseDeploymentKeyword(logger, arg)
		// Skip master references that point to the same node (not applicable for cache)
		if mstr, ok := deployment_keyword.Args["master"]; ok && mstr != "" {
			// For cache, we can keep master but it won't be used during build
		}
		ansible_deployment_args = ansible_deployment_args + " " + handleDeploymentKeyword(logger, "ansible_arguments", arg)
	}

	// Add cache-specific extra vars
	ansible_deployment_args = ansible_deployment_args + " extra_sync_is_required='0' extra_install_only='1'"

	// Generate inventory line (similar to deployHost but for local connection)
	inventoryLine := fmt.Sprintf("localhost ansible_connection=local ansible_python_interpreter=/usr/bin/python3 ansible_ssh_common_args='-o StrictHostKeyChecking=no ' %s", strings.TrimSpace(ansible_deployment_args))

	// Create a sanitized copy of the inventory line for hashing (ignore volatile keys)
	sanitizedInventoryLine := inventoryLine
	excludedKeys := []string{
		"extra_cluster_name",
		"extra_db_password",
		"extra_db_user",
		"extra_master_ip",
		"extra_postgresql_version",
		"extra_start_db",
		"extra_mongo_replicaset",
		"extra_mongo_shardsrv",
		"extra_mongo_configsrv",
		"extra_mongos_shard",
		"extra_mongos_cfg",
	}
	for _, key := range excludedKeys {
		re := regexp.MustCompile(`\s*` + regexp.QuoteMeta(key) + `='[^']*'`)
		sanitizedInventoryLine = re.ReplaceAllString(sanitizedInventoryLine, "")
	}

	// Create a hash from the sanitized inventory line and OS version (so cache is insensitive to excluded keys)
	hashInput := fmt.Sprintf("%s:%s", osver, strings.TrimSpace(sanitizedInventoryLine))
	hash := computeSHA256(hashInput)
	safeUser := strings.ReplaceAll(user, ".", "-")
	cacheImageName := fmt.Sprintf("%s/anydbver-cache:%s", safeUser, hash)

	// Check if image already exists
	args := []string{"docker", "images", "-q", cacheImageName}
	env := map[string]string{}
	errMsg := "Error checking cache image"
	ignoreMsg := regexp.MustCompile("ignore this")

	output, err := runtools.RunGetOutput(logger, args, errMsg, ignoreMsg, false, env, runtools.COMMAND_TIMEOUT)
	if err == nil && strings.TrimSpace(output) != "" {
		logger.Printf("Cache image %s already exists, skipping build\n", cacheImageName)
		return cacheImageName
	}

	// Base64 encode the inventory line to safely pass it as a build argument
	inventoryLineEncoded := base64.StdEncoding.EncodeToString([]byte(inventoryLine))

	// Find Dockerfile.anydbver.cache - check in current working directory first
	cwd, err := os.Getwd()
	if err != nil {
		logger.Printf("Warning: Could not get current working directory, using basic OS image\n")
		return ""
	}

	dockerfilePath := filepath.Join(cwd, "Dockerfile.anydbver.cache")
	var tmpDockerfile *os.File
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		// Try in the config directory
		configDir := filepath.Dir(anydbver_common.GetConfigPath(logger))
		dockerfilePath = filepath.Join(configDir, "Dockerfile.anydbver.cache")
		if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
			logger.Printf("No Dockerfile.anydbver.cache, so no cache image for %v: this node installs its packages from scratch, which takes several minutes\n", deployArgs)
			return ""
		} else {
			// Use config directory as build context if Dockerfile is there
			cwd = configDir
		}
	}

	// Get base image for the OS version
	baseImage := getBaseImageForOS(osver)

	// Pass the base64-encoded inventory line and base image as build arguments
	buildArgs := []string{
		"docker", "build",
		"-t", cacheImageName,
		"--build-arg", "BASE_IMAGE=" + baseImage,
		"--build-arg", "ANSIBLE_INVENTORY_B64=" + inventoryLineEncoded,
		"-f", dockerfilePath,
		cwd,
	}

	logger.Printf("Building cache image for %v, first time for this combination so it installs packages and takes several minutes (later deploys reuse it)\n", deployArgs)

	errMsg = "Error building cache image"
	if rc := runtools.RunFatal(logger, buildArgs, errMsg, ignoreMsg, true, env); rc != 0 {
		logger.Printf("Cache image build failed or timed out, falling back to base image\n")
		return ""
	}

	// Clean up temporary Dockerfile if we created one
	if tmpDockerfile != nil {
		if err := os.Remove(tmpDockerfile.Name()); err != nil {
			logger.Printf("Warning: Could not remove temporary Dockerfile %s: %v\n", tmpDockerfile.Name(), err)
		}
	}

	return cacheImageName
}

func createContainer(logger *log.Logger, config ContainerConfig) {
	name := config.Name
	osver := config.OSVersion
	privileged := config.Privileged
	expose_port := config.ExposePort
	provider := config.Provider
	namespace := config.Namespace
	user := anydbver_common.GetUser(logger)

	// --keep means "add to the existing environment", so a node that is already
	// there is reused. Without this docker run fails on the duplicate name and
	// takes the whole deploy down with it.
	if config.Keep {
		containerName := anydbver_common.MakeContainerHostName(logger, namespace, name)
		if exists, running := anydbver_common.ContainerState(logger, containerName); exists {
			if !running {
				logger.Printf("Starting existing container %s (--keep)\n", containerName)
				env := map[string]string{}
				ignoreMsg := regexp.MustCompile("ignore this")
				runtools.RunFatal(logger, []string{"docker", "start", containerName}, "Error starting container", ignoreMsg, true, env)
			} else {
				logger.Printf("Reusing running container %s (--keep)\n", containerName)
			}
			return
		}
	}

	fmt.Printf("Creating container with name %s, OS %s, privileged=%t, provider=%s, namespace=%s...\n", name, osver, privileged, provider, namespace)

	args := []string{
		"docker", "run",
		"--name", anydbver_common.MakeContainerHostName(logger, namespace, name),
		"--platform", "linux/" + runtime.GOARCH,
		"-v", filepath.Dir(anydbver_common.GetConfigPath(logger)) + "/secret:/vagrant/secret:Z",
		"-v", anydbver_common.GetCacheDirectory(logger) + "/data/nfs:/nfs:Z",
		"-d", "--cgroupns=host", "--tmpfs", "/tmp",
		"--network", getNetworkName(logger, namespace),
		"--tmpfs", "/run", "--tmpfs", "/run/lock",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup",
		"--hostname", name,
	}
	if config.Memory != "" {
		args = append(args, "--memory="+config.Memory)
	}
	if config.CPUs != "" {
		args = append(args, "--cpus="+config.CPUs)
	}
	if privileged {
		args = append(args, []string{
			"--privileged",
			"--cap-add", "NET_ADMIN",
			"--cap-add", "SYS_PTRACE",
			"--cap-add", "IPC_LOCK",
			"--cap-add", "DAC_OVERRIDE",
			"--cap-add", "AUDIT_WRITE",
			"--security-opt", "seccomp=unconfined",
		}...)
	}
	if len(expose_port) > 0 {
		args = append(args, []string{"-p", expose_port}...)
	}

	// Use cache image if DeployArgs is provided, otherwise use basic OS image
	// Cache images only apply to ansible (docker) provider, not kubectl/k3d
	imageName := anydbver_common.GetDockerImageName(osver, user)
	if len(config.DeployArgs) > 0 && config.Provider != "kubectl" {
		cacheImageName := buildCacheImage(logger, config.DeployArgs, osver, user)
		if cacheImageName != "" {
			imageName = cacheImageName
			logger.Printf("Using cache image %s for deployment %v\n", cacheImageName, config.DeployArgs)
		}
	}

	args = append(args, imageName)
	env := map[string]string{}
	errMsg := "Error creating container"
	ignoreMsg := regexp.MustCompile("ignore this")
	runtools.RunFatal(logger, args, errMsg, ignoreMsg, true, env)
}

func shellExec(logger *log.Logger, provider, namespace string, args []string) {
	name := "node0"
	if len(args) > 0 {
		name = args[0]
	}
	cluster_context := ""
	cluster_name := anydbver_common.MakeContainerHostName(logger, namespace, name)
	clusterIp, err := getContainerIp("docker", logger, namespace, "k3d-"+cluster_name+"-"+"server-0")
	if err != nil {
		clusterIp = ""
	} else {
		cluster_context = "k3d-" + cluster_name
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		logger.Println("Error: Could not determine user's home directory")
		return
	}

	volumes := []string{
		"-v", filepath.Dir(anydbver_common.GetConfigPath(logger)) + "/secret:/vagrant/secret:Z",
		"-v", filepath.Join(homeDir, ".kube") + ":/vagrant/secret/.kube:Z",
		"-v", filepath.Join(homeDir, ".config", "gcloud") + ":/vagrant/secret/gcloud:Z",
		"-v", filepath.Join(anydbver_common.GetCacheDirectory(logger), "data") + ":/vagrant/data:Z",
	}
	fix_k3d_config := ""
	if clusterIp != "" {
		fix_k3d_config = "sed -i -re 's/0.0.0.0:[0-9]+/" + clusterIp + ":6443/g' /root/.kube/config ;"
		fix_k3d_config += "kubectl config use-context " + cluster_context + ";"
	}

	userId := "0"
	if user, err := user.Current(); err == nil {
		if _, err := strconv.Atoi(user.Uid); err == nil {
			userId = user.Uid
		}
	}

	ansible_output, err := anydbver_common.RunCommandInBaseContainer(
		logger, namespace,
		"cd /vagrant;mkdir /root/.kube ; cp /vagrant/secret/.kube/config /root/.kube/config; mkdir -p /root/.config; cp -r /vagrant/secret/gcloud /root/.config/; test -f /usr/local/bin/kubectl || (curl -LO https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/"+runtime.GOARCH+"/kubectl ; chmod +x kubectl ; mv kubectl /usr/local/bin/kubectl); test -f /vagrant/tools/yq || (curl -LO  https://github.com/mikefarah/yq/releases/latest/download/yq_linux_"+runtime.GOARCH+" ; chmod +x yq_linux_"+runtime.GOARCH+"; mv yq_linux_"+runtime.GOARCH+" tools/yq); useradd -m -u "+userId+" anydbver; mkdir -p /vagrant/data/k8s; git config --global http.postBuffer 524288000; git config --global --add safe.directory '*'; "+fix_k3d_config+"bash -il",
		volumes,
		"Error running kubernetes operator", true)
	if err != nil {
		logger.Println("Ansible failed with errors: ")
		fatalPattern := regexp.MustCompile(`^fatal:.*$`)
		scanner := bufio.NewScanner(strings.NewReader(ansible_output))
		for scanner.Scan() {
			line := scanner.Text()
			if fatalPattern.MatchString(line) {
				logger.Print(line)
			}
		}
		os.Exit(runtools.ANYDBVER_ANSIBLE_PROBLEM)
	}
}

func containerExec(logger *log.Logger, provider, namespace string, args []string) {
	name := "node0"

	if len(args) <= 1 {
		if len(args) == 1 && args[0] != "--" {
			name = args[0]
		}
		args = []string{"--", "/bin/bash", "--login"}
	} else if len(args) > 1 {
		name = args[0]
		args = args[1:]
	}

	if len(args) > 1 && args[0] == "--" {
		args = args[1:]
	}

	if provider == "docker" {
		docker_args := []string{
			"docker", "exec",
		}

		if term.IsTerminal(int(os.Stdin.Fd())) {
			docker_args = append(docker_args, "-it")
		} else {
			docker_args = append(docker_args, "-i")
		}

		docker_args = append(docker_args, anydbver_common.MakeContainerHostName(logger, namespace, name))

		docker_args = append(docker_args, args...)

		command := exec.Command(docker_args[0], docker_args[1:]...)

		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr

		if err := command.Start(); err != nil {
			log.Fatalf("Failed to start command: %v", err)
		}

		err := command.Wait()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				os.Exit(exitErr.ExitCode())
			} else {
				log.Fatalf("Command finished with error: %v", err)
			}
		}
		os.Exit(0)
	}
}

func ExecuteQueries(dbFile string, table string, deployCmd string, values map[string]string) (string, error) {
	// Open the SQLite3 database
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return "", fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	// Create the temporary table
	_, err = db.Exec(`CREATE TEMPORARY TABLE provided_subcmd(subcmd TEXT, val TEXT);`)
	if err != nil {
		return "", fmt.Errorf("failed to create temporary table: %w", err)
	}

	// Prepare the insert statement
	stmt, err := db.Prepare(`INSERT INTO provided_subcmd(subcmd, val) VALUES (?, ?);`)
	if err != nil {
		return "", fmt.Errorf("failed to prepare insert statement: %w", err)
	}
	defer stmt.Close()

	// Insert values into the temporary table
	for subcmd, val := range values {
		_, err = stmt.Exec(subcmd, val)
		if err != nil {
			return "", fmt.Errorf("failed to insert values: %w", err)
		}
	}

	// Execute the select query
	query := `
		SELECT aa.arg || CASE COALESCE(NULLIF(ps.val,''),aa.arg_default)  WHEN '' THEN '' ELSE "='" || COALESCE(NULLIF(ps.val,''),aa.arg_default)  ||"'" END as arg_val
		FROM ` + table + ` aa
		LEFT JOIN provided_subcmd ps ON aa.subcmd = ps.subcmd
		WHERE aa.cmd=? AND (always_add OR aa.subcmd = ps.subcmd) AND ( (ps.val is not null AND ps.val LIKE aa.version_filter) or ? LIKE aa.version_filter )
		GROUP BY arg
		HAVING orderno = max(orderno);
	`

	rows, err := db.Query(query, deployCmd, values["version"])
	if err != nil {
		return "", fmt.Errorf("failed to execute select query: %w", err)
	}
	defer rows.Close()

	// Collect the results into a string
	var result []string
	for rows.Next() {
		var argVal string
		if err := rows.Scan(&argVal); err != nil {
			return "", fmt.Errorf("failed to scan row: %w", err)
		}
		result = append(result, argVal)
	}
	if err = rows.Err(); err != nil {
		return "", fmt.Errorf("error iterating over rows: %w", err)
	}

	// Join the results with a space
	return strings.Join(result, " "), nil
}

type DeploymentKeywordData struct {
	Cmd  string
	Args map[string]string
}

func IsDeploymentVersion(arg string) bool {
	if strings.HasPrefix(arg, "node") {
		return true
	}
	if strings.HasPrefix(arg, "v") {
		arg = strings.TrimPrefix(arg, "v")
	}

	if strings.HasPrefix(arg, "main") {
		return true
	}

	if len(arg) != 0 && unicode.IsDigit(rune(arg[0])) {
		return true
	}

	return false
}

// versionLess reports whether dotted-numeric version a is older than b.
// A leading 'v' and any '-'/'+' suffix are ignored; missing or non-numeric
// components count as 0 (so "0.1.23" < "0.1.37" and "0.1.9" < "0.1.10").
func versionLess(a, b string) bool {
	norm := func(s string) []int {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		if i := strings.IndexAny(s, "-+"); i >= 0 {
			s = s[:i]
		}
		parts := strings.Split(s, ".")
		nums := make([]int, len(parts))
		for i, p := range parts {
			nums[i], _ = strconv.Atoi(strings.TrimSpace(p))
		}
		return nums
	}
	av, bv := norm(a), norm(b)
	for i := 0; i < len(av) || i < len(bv); i++ {
		var x, y int
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

func ReadDatabaseVersion(dbFile string) (string, error) {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return "", fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	query := `select version from general_version where program='anydbver' order by version desc LIMIT 1`

	rows, err := db.Query(query)
	if err != nil {
		return "", fmt.Errorf("failed to execute select query: %w", err)
	}
	defer rows.Close()

	// Collect the results into a string
	result := ""
	for rows.Next() {
		var argVal string
		if err := rows.Scan(&argVal); err != nil {
			return "", fmt.Errorf("failed to scan row: %w", err)
		}
		result = argVal
	}
	if err = rows.Err(); err != nil {
		return "", fmt.Errorf("error iterating over rows: %w", err)
	}

	// Join the results with a space
	return result, nil
}

func ResolveAlias(tbl string, dbFile string, deployCmd string) (string, error) {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return "", fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	query := `SELECT keyword FROM ` + tbl + ` WHERE alias = ? ORDER BY 1 LIMIT 1`

	rows, err := db.Query(query, deployCmd)
	if err != nil {
		return "", fmt.Errorf("failed to execute select query: %w", err)
	}
	defer rows.Close()

	// Collect the results into a string
	result := deployCmd
	for rows.Next() {
		var argVal string
		if err := rows.Scan(&argVal); err != nil {
			return "", fmt.Errorf("failed to scan row: %w", err)
		}
		result = argVal
	}
	if err = rows.Err(); err != nil {
		return "", fmt.Errorf("error iterating over rows: %w", err)
	}

	// Join the results with a space
	return result, nil
}

func ParseDeploymentKeyword(logger *log.Logger, keyword string) DeploymentKeywordData {
	args := make(map[string]string)
	parts := strings.SplitN(keyword, ":", 2)
	deployCmd := parts[0]
	if len(parts) > 1 {
		keyword = parts[1]
	} else {
		keyword = ""
	}
	if alias, err := ResolveAlias("keyword_aliases", anydbver_common.GetDatabasePath(logger), deployCmd); err == nil {
		deployCmd = alias
	}

	if deployCmd == "mongos-shard" || deployCmd == "mongos-cfg" || deployCmd == "haproxy-pg" || deployCmd == "haproxy-patroni" || deployCmd == "pgbouncer" {
		args["version"] = keyword
		return DeploymentKeywordData{
			Cmd:  deployCmd,
			Args: args,
		}
	}

	pairs := strings.Split(keyword, ",")
	for i, pair := range pairs {
		if i == 0 && IsDeploymentVersion(pair) {
			args["version"] = pair
		} else if i == 0 {
			args["version"] = "latest"
		}

		keyValue := strings.SplitN(pair, "=", 2)

		if alias, err := ResolveAlias("subcmd_aliases", anydbver_common.GetDatabasePath(logger), keyValue[0]); err == nil {
			keyValue[0] = alias
		}

		if len(keyValue) == 1 {
			key := keyValue[0]
			args[key] = ""
		}
		if len(keyValue) == 2 {
			key := keyValue[0]
			value := keyValue[1]
			args[key] = value
		}
	}

	// coroot ships only as a docker image, there is no ansible role for it, so
	// it selects the docker-image provider on its own instead of making the
	// user write "coroot:docker-image".
	if deployCmd == "coroot-server" {
		if _, ok := args["docker-image"]; !ok {
			args["docker-image"] = ""
		}
	}

	return DeploymentKeywordData{
		Cmd:  deployCmd,
		Args: args,
	}
}

func handleDBPreReq(logger *log.Logger, namespace string, name string, cmd string, args map[string]string) {
	if cmd == "percona-server-mongodb" {
		unmodified_docker.SetupMongoKeyFiles(logger, namespace, anydbver_common.MakeContainerHostName(logger, namespace, name), args)
	} else if cmd == "percona-xtradb-cluster" {
	}
}

func handleDeploymentKeyword(logger *log.Logger, table string, keyword string) string {
	deployment_keyword := ParseDeploymentKeyword(logger, keyword)
	return handleDeploymentKeywordParsed(logger, table, deployment_keyword)
}

func handleDeploymentKeywordParsed(logger *log.Logger, table string, deployment_keyword DeploymentKeywordData) string {
	if (table == "ansible_arguments" || table == "k8s_arguments") && deployment_keyword.Args["version"] == "latest" {
		delete(deployment_keyword.Args, "version")
	}
	result, err := ExecuteQueries(anydbver_common.GetDatabasePath(logger), table, deployment_keyword.Cmd, deployment_keyword.Args)
	if err != nil {
		logger.Fatalf("Error: %v", err)
		return ""
	}
	logger.Println(result)
	return result
}

func runOperatorTool(logger *log.Logger, namespace string, name string, run_operator_args string) {
	if run_operator_args == "" {
		return
	}
	cluster_context := ""
	cluster_name := anydbver_common.MakeContainerHostName(logger, namespace, name)
	clusterIp, err := getContainerIp("docker", logger, namespace, "k3d-"+cluster_name+"-"+"server-0")
	if err != nil {
		clusterIp = ""
	} else {
		cluster_context = "k3d-" + cluster_name
	}
	homeDir, err := os.UserHomeDir()
	if err != nil {
		logger.Println("Error: Could not determine user's home directory")
		return
	}

	volumes := []string{
		"-v", filepath.Dir(anydbver_common.GetConfigPath(logger)) + "/secret:/vagrant/secret:Z",
		"-v", filepath.Join(homeDir, ".kube") + ":/vagrant/secret/.kube:Z",
		"-v", filepath.Join(homeDir, ".config", "gcloud") + ":/vagrant/secret/gcloud:Z",
		"-v", filepath.Join(anydbver_common.GetCacheDirectory(logger), "data") + ":/vagrant/data:Z",
	}
	fix_k3d_config := ""
	if clusterIp != "" {
		fix_k3d_config = "sed -i -re 's/0.0.0.0:[0-9]+/" + clusterIp + ":6443/g' /root/.kube/config ;"
		fix_k3d_config += "kubectl config use-context " + cluster_context + ";"
	}

	userId := "0"
	if user, err := user.Current(); err == nil {
		if _, err := strconv.Atoi(user.Uid); err == nil {
			userId = user.Uid
		}
	}

	ansible_output, err := anydbver_common.RunCommandInBaseContainerWithTimeout(
		logger, namespace,
		"source ~/.bashrc;cd /vagrant;mkdir /root/.kube ; cp /vagrant/secret/.kube/config /root/.kube/config; mkdir -p /root/.config; cp -r /vagrant/secret/gcloud /root/.config/; test -f /usr/local/bin/kubectl || (curl -LO https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/"+runtime.GOARCH+"/kubectl ; chmod +x kubectl ; mv kubectl /usr/local/bin/kubectl); test -f /vagrant/tools/yq || (curl -LO  https://github.com/mikefarah/yq/releases/latest/download/yq_linux_"+runtime.GOARCH+" ; chmod +x yq_linux_"+runtime.GOARCH+"; mv yq_linux_"+runtime.GOARCH+" tools/yq); useradd -m -u "+userId+" anydbver; mkdir -p /vagrant/data/k8s; git config --global http.postBuffer 524288000; git config --global --add safe.directory '*'; "+fix_k3d_config+"python3 tools/run_k8s_operator.py "+run_operator_args+"; operator_rc=$?; chown -R "+userId+" /vagrant/data/k8s/; exit $operator_rc",
		volumes,
		"Error running kubernetes operator", false, runtools.COMMAND_TIMEOUT*6)
	if err != nil {
		logger.Println("Ansible failed with errors: ")
		fatalPattern := regexp.MustCompile(`^fatal:.*$`)
		scanner := bufio.NewScanner(strings.NewReader(ansible_output))
		for scanner.Scan() {
			line := scanner.Text()
			if fatalPattern.MatchString(line) {
				logger.Print(line)
			}
		}
		os.Exit(runtools.ANYDBVER_ANSIBLE_PROBLEM)
	}

	// PMM HA exposes everything as ClusterIP and HAProxy is the only supported
	// entry point, so the UI is unreachable from the host without a tunnel.
	// Start a background host-side port-forward (same approach as Everest) so
	// the user can connect right after a successful deploy. Failure here is
	// non-fatal: we fall back to printing the manual command.
	if clusterIp != "" && strings.Contains(run_operator_args, "--pmm-ha=") {
		k8s_namespace := "pmm"
		if m := regexp.MustCompile(`--namespace='([^']*)'`).FindStringSubmatch(run_operator_args); m != nil {
			k8s_namespace = m[1]
		}
		pmm_password := "admin"
		if m := regexp.MustCompile(`--pmm-ha-password='([^']*)'`).FindStringSubmatch(run_operator_args); m != nil {
			pmm_password = m[1]
		}
		local_port := "8443"

		// HAProxy is a separate Deployment from the pmm-ha StatefulSet the
		// deploy waits on; it commonly flips Pending->Running only as the PMM
		// servers come up. If we forward to it while it's still Pending,
		// kubectl exits immediately ("pod is not running, status=Pending"), so
		// wait for it to be Available first (best-effort, host kubectl).
		logger.Printf("Waiting for HAProxy to be ready before starting port-forward...")
		exec.Command("kubectl", "wait", "--for=condition=Available",
			"--timeout=180s", "-n", k8s_namespace, "deployment/pmm-ha-haproxy",
			"--context", cluster_context).Run()

		logger.Printf("Starting port-forward to PMM HA UI...")
		kubectl_portforward := exec.Command("kubectl", "port-forward",
			"-n", k8s_namespace, "svc/pmm-ha-haproxy", local_port+":443",
			"--address", "0.0.0.0", "--context", cluster_context)
		// Detach into a new session so the tunnel outlives `anydbver` exiting
		// (terminal SIGHUP on close, parent process-group reaping, etc.) — a
		// plain Start() child gets reaped with us and the tunnel dies.
		kubectl_portforward.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		// Don't inherit our stdio: a write to a closed terminal would kill the
		// detached child. Log to a file in the cache dir instead.
		pf_log_path := filepath.Join(anydbver_common.GetCacheDirectory(logger), "pmm-ha-port-forward.log")
		if pf_log, logErr := os.OpenFile(pf_log_path, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644); logErr == nil {
			kubectl_portforward.Stdout = pf_log
			kubectl_portforward.Stderr = pf_log
			defer pf_log.Close()
		}
		pf_err := kubectl_portforward.Start()

		fmt.Println("")
		fmt.Println("===============================================")
		fmt.Println("PMM HA deployed successfully!")
		fmt.Println("===============================================")
		fmt.Println("")
		fmt.Printf("PMM UI:   https://localhost:%s\n", local_port)
		fmt.Println("Username: admin")
		fmt.Printf("Password: %s\n", pmm_password)
		fmt.Println("")
		if pf_err == nil && kubectl_portforward.Process != nil {
			fmt.Println("Port-forward is running in background (PID:", kubectl_portforward.Process.Pid, ")")
			fmt.Println("To stop: kill", kubectl_portforward.Process.Pid)
			fmt.Println("Log:    ", pf_log_path)
		} else {
			fmt.Println("Could not start port-forward automatically:", pf_err)
			fmt.Println("(is kubectl installed on the host?)")
		}
		fmt.Println("")
		fmt.Println("To (re)start the port-forward manually:")
		fmt.Printf("  kubectl port-forward -n %s svc/pmm-ha-haproxy %s:443 --context %s\n", k8s_namespace, local_port, cluster_context)
		fmt.Println("")
	}
}

func deployHost(provider string, logger *log.Logger, namespace string, name string, ansible_hosts_run_file string, args []string, keep bool) {
	if provider == "docker-image" {
		// Same reasoning as createContainer: a docker-image node that already
		// exists is reused rather than recreated. Products with sidecars, like
		// coroot, create them together with the main container, so the main one
		// existing means the whole node is there.
		if keep {
			containerName := anydbver_common.MakeContainerHostName(logger, namespace, name)
			if exists, running := anydbver_common.ContainerState(logger, containerName); exists {
				if !running {
					env := map[string]string{}
					ignoreMsg := regexp.MustCompile("ignore this")
					runtools.RunFatal(logger, []string{"docker", "start", containerName}, "Error starting container", ignoreMsg, true, env)
				}
				logger.Printf("Reusing existing %s (--keep)\n", containerName)
				return
			}
		}
		for _, arg := range args {
			deployment_keyword := ParseDeploymentKeyword(logger, arg)
			// coroot-client creates no container of its own, it registers the
			// database with a coroot server after the deploy, so it is allowed
			// next to a docker-image database.
			if deployment_keyword.Cmd == "coroot-client" {
				continue
			}
			if _, ok := deployment_keyword.Args["docker-image"]; ok {
				unmodified_docker.CreateContainer(logger, namespace, name, deployment_keyword.Cmd, deployment_keyword.Args)
			} else {
				logger.Printf("Can't mix docker-image items with non-docker. Please keep only a single docker-image command per node.\n Problem with node %s and definition %v", name, args)
				os.Exit(runtools.ANYDBVER_DOCKER_IMAGE_MIXED_WITH_ANSIBLE)
			}
		}
	} else if provider == "kubectl" {
		run_operator_args := ""

		for _, arg := range args {
			deployment_keyword_args := handleDeploymentKeyword(logger, "k8s_arguments", arg)
			if !strings.Contains(deployment_keyword_args, "--version") {
				run_operator_args = run_operator_args + " " + deployment_keyword_args
			}
		}

		runOperatorTool(logger, namespace, name, run_operator_args)

		for _, arg := range args {
			deployment_keyword_args := handleDeploymentKeyword(logger, "k8s_arguments", arg)
			if !strings.Contains(deployment_keyword_args, "--version") {
				continue
			}

			runOperatorTool(logger, namespace, name, run_operator_args+" "+deployment_keyword_args)
		}

	} else if provider == "docker" {
		logger.Printf("Deploy %v", args)
		file, err := os.OpenFile(ansible_hosts_run_file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Println("Error opening file:", err)
			return
		}
		defer file.Close()

		ip, err := getNodeIp(provider, logger, namespace, name)

		ansible_deployment_args := ""

		for _, arg := range args {
			deployment_keyword := ParseDeploymentKeyword(logger, arg)
			if mstr, ok := deployment_keyword.Args["master"]; ok && mstr == name {
				logger.Printf("A master can't lead itself: %s: %s", name, arg)
				delete(deployment_keyword.Args, "master")
			}
			handleDBPreReq(logger, namespace, name, deployment_keyword.Cmd, deployment_keyword.Args)
			ansible_deployment_args = ansible_deployment_args + " " + handleDeploymentKeywordParsed(logger, "ansible_arguments", deployment_keyword)
		}

		content := anydbver_common.MakeContainerHostName(logger, namespace, name) + " ansible_connection=ssh ansible_user=root ansible_ssh_private_key_file=secret/id_rsa ansible_host=" + ip + " ansible_python_interpreter=/usr/bin/python3 ansible_ssh_common_args='-o StrictHostKeyChecking=no ' " + ansible_deployment_args + "\n"

		for {
			re_mongo_shard_hosts := regexp.MustCompile(`(extra_mongos_shard|extra_mongos_cfg|extra_haproxy_pg|extra_haproxy_patroni|extra_pgbouncer|extra_patroni_standby)='([^']*)(node[0-9]+)([^']*)'`)
			content_with_ip := re_mongo_shard_hosts.ReplaceAllStringFunc(content, func(match string) string {
				submatches := re_mongo_shard_hosts.FindStringSubmatch(match)
				if len(submatches) > 4 {
					node := submatches[3]
					ip, err := getNodeIp(provider, logger, namespace, node)
					if err != nil {
						fmt.Println("Error getting node ip:", err)
					}

					return fmt.Sprintf("%s='%s%s%s'", submatches[1], submatches[2], ip, submatches[4])
				}
				return match
			})
			if content_with_ip == content {
				break
			}
			content = content_with_ip
		}

		re_pmm_server := regexp.MustCompile(`(extra_pmm_url)='(node[0-9]+)([:][0-9]+)?'`)
		content = re_pmm_server.ReplaceAllStringFunc(content, func(match string) string {
			submatches := re_pmm_server.FindStringSubmatch(match)
			if len(submatches) > 2 {
				node := submatches[2]
				ip, err := getNodeIp(provider, logger, namespace, node)
				if err != nil {
					fmt.Println("Error getting node ip:", err)
				}

				port := submatches[3]
				if port == "" {
					port = getContainerPort(logger, namespace, node)
				}

				return fmt.Sprintf("%s='https://admin:%s@%s%s'", submatches[1], url.QueryEscape(anydbver_common.ANYDBVER_DEFAULT_PASSWORD), ip, port)
			}
			return match
		})

		re_s3_server := regexp.MustCompile(`(extra_minio_url|extra_pbm_s3_url)='(node[0-9]+)(/[^']*)?'`)
		content = re_s3_server.ReplaceAllStringFunc(content, func(match string) string {
			submatches := re_s3_server.FindStringSubmatch(match)
			if len(submatches) > 2 {
				kwd := submatches[1]
				node := submatches[2]
				bucket := ""
				if len(submatches) > 3 {
					bucket = submatches[3]
				}

				return fmt.Sprintf("%s='https://%s:%s@%s:9000%s'",
					kwd,
					url.QueryEscape(anydbver_common.ANYDBVER_MINIO_USER),
					url.QueryEscape(anydbver_common.ANYDBVER_MINIO_PASS),
					anydbver_common.MakeContainerHostName(logger, namespace, node), bucket)
			}
			return match
		})

		re := regexp.MustCompile(`='(node[0-9]+)'`)
		content = re.ReplaceAllStringFunc(content, func(match string) string {
			submatches := re.FindStringSubmatch(match)
			if len(submatches) > 1 {
				node := submatches[1]
				ip, err := getNodeIp(provider, logger, namespace, node)
				if err != nil {
					fmt.Println("Error getting node ip:", err)
				}

				return fmt.Sprintf("='%s'", ip)
			}
			return match
		})

		_, err = file.WriteString(content)
		if err != nil {
			fmt.Println("Error writing to file:", err)
			return
		}
	}
}

func extractLastPart(s string) string {
	if strings.Contains(s, ":") {
		afterColon := strings.Split(s, ":")[1]
		parts := strings.Split(afterColon, ",")
		return parts[len(parts)-1]
	}
	return ""
}

func replaceLastOccurrence(toComplete, keywordPart string) string {
	index := strings.LastIndex(toComplete, keywordPart)
	if index != -1 {
		return toComplete[:index] + toComplete[index+len(keywordPart):]
	}
	return toComplete
}

func fetchDeployCompletions(logger *log.Logger, toComplete string) []string {
	var keywords []string

	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		logger.Println("failed to open database:", err)
		return keywords
	}
	defer db.Close()

	f, err := os.OpenFile("/tmp/anydbver.log", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("error opening file: %v", err)
	}
	defer f.Close()
	logger.SetOutput(f)

	query := `select distinct keyword from (select keyword from keyword_aliases union select alias as keyword from keyword_aliases union select cmd from ansible_arguments union select cmd from k8s_arguments) a order by keyword`

	if strings.HasSuffix(toComplete, ":") {
		keyword := strings.SplitN(toComplete, ":", 2)[0]
		query = fmt.Sprintf(
			`select concat('%s',(case when version_filter <> '%%' THEN concat(subcmd,'=',version_filter) ELSE subcmd END)) from ansible_arguments where cmd=(select keyword from keyword_aliases where alias='%s' LIMIT 1) and subcmd like '%s%%'`,
			replaceLastOccurrence(toComplete, extractLastPart(toComplete)), keyword, extractLastPart(toComplete),
		)

	}

	logger.Println("Got a command to complete:", toComplete, " Query: ", query)

	rows, err := db.Query(query)
	if err != nil {
		logger.Println("failed to execute select query:", err, query)
		return keywords
	}
	defer rows.Close()

	for rows.Next() {
		var keyword string
		if err := rows.Scan(&keyword); err != nil {
			logger.Println("failed to scan row:", err)
			return keywords
		}
		keywords = append(keywords, keyword)
	}
	if err = rows.Err(); err != nil {
		logger.Println("error iterating over rows:", err)
		return keywords
	}

	return keywords
}

func runPlaybook(logger *log.Logger, namespace string, ansible_hosts_run_file string, verbose bool) {
	user := anydbver_common.GetUser(logger)

	fileInfo, err := os.Stat(ansible_hosts_run_file)
	if os.IsNotExist(err) || (err == nil && fileInfo.Size() == 0) {
		logger.Println("no traditional installations with systemd, skipping ansible")
		return
	}
	if err != nil {
		logger.Println("Can't stat ansible hosts file", err)
		return
	}

	volumes := []string{
		"-v", ansible_hosts_run_file + ":/vagrant/ansible_hosts_run:Z",
		"-v", anydbver_common.GetDatabasePath(logger) + ":/vagrant/anydbver_version.db:Z",
		"-v", filepath.Dir(anydbver_common.GetConfigPath(logger)) + "/secret:/vagrant/secret:Z",
	}

	// Try current directory first, then parent directory (for when binary is in tools/),
	// then fall back to the directory of the binary itself
	projectRoot := ""
	if dirInfo, err := os.Stat("roles"); err == nil && dirInfo.IsDir() {
		projectRoot, _ = filepath.Abs(".")
	} else if dirInfo, err := os.Stat("../roles"); err == nil && dirInfo.IsDir() {
		projectRoot, _ = filepath.Abs("..")
	} else if exePath, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exePath)
		if dirInfo, err := os.Stat(filepath.Join(exeDir, "roles")); err == nil && dirInfo.IsDir() {
			projectRoot = exeDir
		} else if dirInfo, err := os.Stat(filepath.Join(filepath.Dir(exeDir), "roles")); err == nil && dirInfo.IsDir() {
			projectRoot = filepath.Dir(exeDir)
		}
	}
	if projectRoot != "" {
		for _, dir := range []struct{ src, dest string }{
			{"roles", "/vagrant/roles"},
			{"common", "/vagrant/common"},
			{"tools", "/vagrant/tools"},
			{"library", "/vagrant/library"},
			{"configs", "/vagrant/configs"},
		} {
			fullPath := filepath.Join(projectRoot, dir.src)
			if di, err := os.Stat(fullPath); err == nil && di.IsDir() {
				volumes = append(volumes, "-v", fullPath+":"+dir.dest+":Z")
			}
		}
		playbookPath := filepath.Join(projectRoot, "playbook.yml")
		if fi, err := os.Stat(playbookPath); err == nil && !fi.IsDir() {
			volumes = append(volumes, "-v", playbookPath+":/vagrant/playbook.yml:Z")
		}
	}

	cmd_args := []string{
		"docker", "run", "-i", "--rm",
		"--name", anydbver_common.MakeContainerHostName(logger, namespace, "ansible"),
		"--network", getNetworkName(logger, namespace),
		"--hostname", anydbver_common.MakeContainerHostName(logger, namespace, "ansible"),
	}

	cmd_args = append(cmd_args, volumes...)
	cmd_args = append(cmd_args, []string{
		anydbver_common.GetDockerImageName("ansible", user),
		"bash", "-c",
	}...)

	ansible_command := "cd /vagrant;until ansible -m ping -i ansible_hosts_run all &>/dev/null ; do sleep 1; done ; ANSIBLE_FORCE_COLOR=True ANSIBLE_DISPLAY_SKIPPED_HOSTS=False ansible-playbook -i ansible_hosts_run --forks 16 playbook.yml"

	if verbose {
		ansible_command += " -vvv"
	}

	cmd_args = append(cmd_args, ansible_command)

	env := map[string]string{}
	errMsg := "Error running Ansible"
	ignoreMsg := regexp.MustCompile("ignore this")
	ansible_output, err := runtools.RunPipe(logger, cmd_args, errMsg, ignoreMsg, true, env, 1200)
	if err != nil {
		logger.Println("Ansible failed with errors: ")
		fatalPattern := regexp.MustCompile(`FAILED[!]|failed=`)
		taskPattern := regexp.MustCompile(`^TASK \[.*\]`)
		lastTask := ""
		scanner := bufio.NewScanner(strings.NewReader(ansible_output))
		for scanner.Scan() {
			line := scanner.Text()
			if taskPattern.MatchString(line) {
				lastTask = line
			}
			if fatalPattern.MatchString(line) {
				logger.Print(line)
			}
		}
		if strings.Contains(err.Error(), "timed out") && lastTask != "" {
			logger.Printf("Timed out during: %s", lastTask)
		}
		os.Exit(runtools.ANYDBVER_ANSIBLE_PROBLEM)
	}
}

func createK3dCluster(logger *log.Logger, namespace string, name string, args map[string]string) {
	cluster_name := anydbver_common.MakeContainerHostName(logger, namespace, name)
	k3d_agents := 2
	if nodes, ok := args["nodes"]; ok {
		if nodes_num, err := strconv.Atoi(nodes); err == nil {
			nodes_num--
			if nodes_num > 0 {
				k3d_agents = nodes_num
			}
		}
	}

	k3d_path, err := anydbver_common.GetK3dPath(logger)
	if err != nil {
		log.Fatalf("Can't create k3d cluster: %v", err)
	}

	k3d_create_cmd := []string{
		k3d_path, "cluster", "create",
		cluster_name,
		"-i", "rancher/k3s:" + args["version"],
		"--network", getNetworkName(logger, namespace),
		"-a", strconv.Itoa(k3d_agents),
	}

	k3d_create_cmd = append(k3d_create_cmd, []string{
		"--k3s-arg", "--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@server:*",
		"--k3s-arg", "--kubelet-arg=eviction-minimum-reclaim=imagefs.available=1%,nodefs.available=1%@server:*",
		"--k3s-arg", "--kubelet-arg=eviction-hard=imagefs.available<1%,nodefs.available<1%@agent:*",
		"--k3s-arg", "--kubelet-arg=eviction-minimum-reclaim=imagefs.available=1%,nodefs.available=1%@agent:*",
	}...)

	if dir, ok := args["storage-path"]; ok {
		k3d_create_cmd = append(k3d_create_cmd, []string{
			"--volume",
			dir + ":/var/lib/rancher/k3s/storage@all",
		}...)
	}

	k3d_create_cmd = append(k3d_create_cmd, []string{
		"--volume",
		"/sys/kernel/debug:/sys/kernel/debug@all",
	}...)

	if host_alias, ok := args["host-alias"]; ok {
		k3d_create_cmd = append(k3d_create_cmd, []string{
			"--host-alias",
			strings.ReplaceAll(host_alias, "|", ","),
		}...)
	}

	if ingress_type, ok := args["ingress-type"]; ok && ingress_type != "traefik" {
		k3d_create_cmd = append(k3d_create_cmd, []string{
			"--k3s-arg",
			"--disable=traefik@server:0",
		}...)
		if ingress_port, ok := args["ingress"]; ok {
			k3d_create_cmd = append(k3d_create_cmd, []string{
				"-p",
				fmt.Sprintf("%s:%s@loadbalancer", ingress_port, ingress_port),
			}...)
		}

	}
	if registry_cache, ok := args["registry-cache"]; ok {
		registry_cache_config := fmt.Sprintf(`
mirrors:
  docker.io:
    endpoint:
    - "%s"
`, registry_cache)
		registry_cache_file := filepath.Join(anydbver_common.GetCacheDirectory(logger), "registry-mirror.yaml")
		file, err := os.OpenFile(registry_cache_file, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			fmt.Println("Error opening file:", err)
			return
		}
		defer file.Close()
		file.WriteString(registry_cache_config)
		k3d_create_cmd = append(k3d_create_cmd, "--registry-config", registry_cache_file)
	}

	env := map[string]string{}
	errMsg := "Error creating k3d cluster"
	ignoreMsg := regexp.MustCompile("ignore this")
	runtools.RunPipe(logger, k3d_create_cmd, errMsg, ignoreMsg, true, env, 600)
}

func checkEverestPrerequisites(logger *log.Logger) {
	missing := []string{}

	// Check docker
	if _, err := exec.LookPath("docker"); err != nil {
		missing = append(missing, "docker")
	} else {
		// Check docker daemon is running
		cmd := exec.Command("docker", "info")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Run(); err != nil {
			logger.Fatal("Docker is installed but the daemon is not running. Please start Docker first.")
		}
	}

	// Check kubectl
	if _, err := exec.LookPath("kubectl"); err != nil {
		missing = append(missing, "kubectl")
	}

	// Check k3d
	if _, err := anydbver_common.GetK3dPath(logger); err != nil {
		missing = append(missing, "k3d")
	}

	if len(missing) > 0 {
		logger.Fatalf("Missing required tools: %s. Please install them before deploying Everest.", strings.Join(missing, ", "))
	}

	logger.Printf("All prerequisites found: docker, kubectl, k3d")
}

func deployEverest(logger *log.Logger, namespace string, nodes int, everestVersion string) {
	checkEverestPrerequisites(logger)

	cluster_name := anydbver_common.MakeContainerHostName(logger, namespace, "cluster1")

	// Create k3d cluster
	logger.Printf("Creating k3d cluster with %d nodes...", nodes)
	k3dArgs := map[string]string{
		"version": "latest",
		"nodes":   strconv.Itoa(nodes),
	}
	createK3dCluster(logger, namespace, "cluster1", k3dArgs)

	// Wait for cluster to be ready
	logger.Printf("Waiting for k3d cluster to be ready...")
	env := map[string]string{}
	errMsg := "Error waiting for k3d cluster"
	ignoreMsg := regexp.MustCompile("ignore this")

	// Get kubeconfig
	homeDir, _ := os.UserHomeDir()
	kubeconfig_path := filepath.Join(homeDir, ".kube", "config")
	os.MkdirAll(filepath.Dir(kubeconfig_path), 0755)
	k3d_path, _ := anydbver_common.GetK3dPath(logger)
	runtools.RunFatal(logger, []string{k3d_path, "kubeconfig", "get", cluster_name}, errMsg, ignoreMsg, true, env)

	// Export kubeconfig
	kubeconfig_cmd := []string{k3d_path, "kubeconfig", "write", cluster_name, "--output", kubeconfig_path}
	runtools.RunFatal(logger, kubeconfig_cmd, errMsg, ignoreMsg, true, env)

	// Get cluster IP for kubeconfig fix
	clusterIp, _ := getContainerIp("docker", logger, namespace, "k3d-"+cluster_name+"-server-0")

	// Install Percona Everest
	logger.Printf("Installing Percona Everest %s...", everestVersion)
	helm_install_everest := []string{
		"docker", "run", "--rm",
		"--network", getNetworkName(logger, namespace),
		"-v", kubeconfig_path + ":/root/.kube/config",
		"alpine/helm:latest",
		"upgrade", "--install", "everest-core", "everest",
		"--repo", "https://percona.github.io/percona-helm-charts/",
		"--namespace", "everest-system",
		"--create-namespace",
		"--kubeconfig", "/root/.kube/config",
		"--kube-apiserver", "https://" + clusterIp + ":6443",
		"--wait", "--timeout", "10m",
	}
	if everestVersion != "latest" && everestVersion != "" {
		helm_install_everest = append(helm_install_everest, "--version", everestVersion)
	}
	runtools.RunPipe(logger, helm_install_everest, "Error installing Percona Everest", ignoreMsg, true, env, 600)

	// Start port-forward in background
	logger.Printf("Starting port-forward to Everest UI...")
	kubectl_portforward := exec.Command("kubectl", "port-forward", "svc/everest", "8080:8080",
		"-n", "everest-system", "--address", "0.0.0.0",
		"--kubeconfig", kubeconfig_path)
	kubectl_portforward.Start()

	// Print access instructions
	fmt.Println("")
	fmt.Println("===============================================")
	fmt.Println("Percona Everest deployed successfully!")
	fmt.Println("===============================================")
	fmt.Println("")
	fmt.Println("Everest UI: http://localhost:8080")
	fmt.Println("Username:   admin")
	fmt.Println("Password:   kubectl get secret everest-accounts -n everest-system -o jsonpath='{.data.users\\.yaml}' | base64 -d")
	fmt.Println("")
	fmt.Println("Port-forward is running in background (PID:", kubectl_portforward.Process.Pid, ")")
	fmt.Println("To stop: kill", kubectl_portforward.Process.Pid)
	fmt.Println("")
	fmt.Println("To restart port-forward manually:")
	fmt.Println("  kubectl port-forward svc/everest 8080:8080 -n everest-system --address 0.0.0.0")
	fmt.Println("")
}

// knownDeployKeywords returns the set of valid deploy keyword commands the CLI
// understands: every product, alias and operator name from the version DB, plus
// the handful of tokens that are interpreted directly in code and are not backed
// by DB rows. Used to reject unknown keywords instead of silently dropping them.
func knownDeployKeywords(logger *log.Logger) map[string]bool {
	known := map[string]bool{
		// Tokens handled directly by deployHosts / ParseDeploymentKeyword, not
		// present as ansible_arguments / k8s_arguments / keyword_aliases rows.
		"os":       true,
		"provider": true,
	}
	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		logger.Printf("Warning: could not open version DB to validate keywords: %v", err)
		return known
	}
	defer db.Close()
	rows, err := db.Query(`select distinct keyword from (select keyword from keyword_aliases union select alias as keyword from keyword_aliases union select cmd from ansible_arguments union select cmd from k8s_arguments) a`)
	if err != nil {
		logger.Printf("Warning: could not query keywords for validation: %v", err)
		return known
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err == nil {
			known[k] = true
		}
	}
	return known
}

// levenshtein returns the edit distance between a and b (used to suggest the
// closest valid keyword for a typo'd one).
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	curr := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		curr[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := curr[j-1] + 1
			sub := prev[j-1] + cost
			min := del
			if ins < min {
				min = ins
			}
			if sub < min {
				min = sub
			}
			curr[j] = min
		}
		prev, curr = curr, prev
	}
	return prev[lb]
}

// closestKeyword returns the known keyword nearest to word (edit distance <= 3),
// or "" if none is close enough.
func closestKeyword(word string, known map[string]bool) string {
	best := ""
	bestDist := 4 // threshold: only suggest within 3 edits
	for k := range known {
		d := levenshtein(word, k)
		if d < bestDist {
			bestDist = d
			best = k
		}
	}
	return best
}

// validateDeployKeywords fails fast if any deploy argument uses a keyword the CLI
// does not recognize. Without this an unknown token (e.g. a typo, or a made-up
// keyword like "galera-master:node1") is silently dropped, producing a broken
// deployment with no diagnostic.
func validateDeployKeywords(logger *log.Logger, args []string) {
	known := knownDeployKeywords(logger)
	var unknown []string
	for _, arg := range args {
		// node separators (node0, node1, ...) are not keywords
		if strings.HasPrefix(arg, "node") {
			continue
		}
		if strings.TrimSpace(arg) == "" {
			continue
		}
		// keyword is the token before ':' (product) and before any ',' options
		cmd := strings.SplitN(strings.SplitN(arg, ":", 2)[0], ",", 2)[0]
		if cmd == "" {
			continue
		}
		if !known[cmd] {
			unknown = append(unknown, arg)
		}
	}
	if len(unknown) == 0 {
		return
	}
	for _, arg := range unknown {
		cmd := strings.SplitN(strings.SplitN(arg, ":", 2)[0], ",", 2)[0]
		logger.Printf("Error: unknown deploy keyword %q (in %q) - it is not a known product, alias or option. It would be silently ignored, leaving a broken deployment.", cmd, arg)
		if strings.Contains(cmd, "master") || strings.Contains(cmd, "galera") || strings.Contains(cmd, "replica") {
			logger.Printf("  Hint: clustering is expressed with comma-separated options ON a product item, not as a separate token.")
			logger.Printf("        e.g. cluster join: 'pxc:latest,master=node0,galera'  (not 'galera-master:node0')")
		} else if sug := closestKeyword(cmd, known); sug != "" {
			logger.Printf("  Did you mean %q?", sug)
		}
	}
	logger.Printf("Run 'anydbver deploy help keywords' to list valid keywords.")
	os.Exit(runtools.ANYDBVER_UNKNOWN_KEYWORD)
}

func deployHosts(logger *log.Logger, ansible_hosts_run_file string, provider string, namespace string, args []string, verbose bool, memory string, cpus string, keep bool) {
	privileged := ""
	re_lastosver := regexp.MustCompile(`=[^=]+$`)
	osvers := "node0=el8"
	nodeDefinitions := make(map[string][]string)
	nodeProvider := make(map[string]string)
	expose_ports := make(map[string]string)
	currentNode := "node0"

	nodeProvider[currentNode] = provider
	for i, arg := range args {
		if strings.HasPrefix(arg, "node") {
			if i == 0 {
				osvers = arg + "=el8"
			} else {
				osvers = osvers + "," + arg + "=el8"
			}

			currentNode = arg
			nodeProvider[currentNode] = provider
		} else {
			if nodeDef, ok := nodeDefinitions[currentNode]; ok {
				nodeDefinitions[currentNode] = append(nodeDef, arg)
			} else {
				nodeDefinitions[currentNode] = []string{arg}
			}
			deployment_keyword := ParseDeploymentKeyword(logger, arg)
			if deployment_keyword.Cmd == "os" {
				osver := strings.TrimPrefix(arg, "os:")
				osvers = re_lastosver.ReplaceAllString(osvers, "="+osver)
			} else if _, ok := deployment_keyword.Args["docker-image"]; ok {
				nodeProvider[currentNode] = "docker-image"
				osvers = re_lastosver.ReplaceAllString(osvers, "")
			} else if port_to_expose, ok := deployment_keyword.Args["expose"]; ok {
				expose_ports[currentNode] = port_to_expose
			} else if deployment_keyword.Cmd == "k3d" {
				nodeProvider[currentNode] = "kubectl"
				osvers = re_lastosver.ReplaceAllString(osvers, "")
				createK3dCluster(logger, namespace, currentNode, deployment_keyword.Args)
			} else if arg == "provider:kubectl" {
				nodeProvider[currentNode] = "kubectl"
				osvers = re_lastosver.ReplaceAllString(osvers, "")
			} else if nodeProvider[currentNode] != "kubectl" &&
				strings.HasSuffix(deployment_keyword.Cmd, "-operator") {
				nodeProvider[currentNode] = "kubectl"
				osvers = re_lastosver.ReplaceAllString(osvers, "")
				deployment_keyword := ParseDeploymentKeyword(logger, "k3d")
				createK3dCluster(logger, namespace, currentNode, deployment_keyword.Args)
			}
		}
	}
	anydbver_common.CreateSshKeysForContainers(logger, namespace)
	containers := []ContainerConfig{}
	priv_map := ConvertStringToMap(privileged)
	for node, value := range ConvertStringToMap(osvers) {
		privileged_container := true
		if val, ok := priv_map[node]; ok {
			if priv, err := strconv.ParseBool(val); err == nil {
				privileged_container = priv
			}
		}
		expose_port := ""
		if ep, ok := expose_ports[node]; ok {
			expose_port = ep
		}

		// Extract all deployment keywords for cache image (skip os, docker-image, k3d, and operator commands)
		deployArgs := []string{}
		if nodeDef, ok := nodeDefinitions[node]; ok {
			for _, arg := range nodeDef {
				deployment_keyword := ParseDeploymentKeyword(logger, arg)
				// Skip special commands that don't need cache images
				if deployment_keyword.Cmd != "os" &&
					deployment_keyword.Cmd != "k3d" &&
					!strings.HasSuffix(deployment_keyword.Cmd, "-operator") {
					if _, isDockerImage := deployment_keyword.Args["docker-image"]; !isDockerImage {
						deployArgs = append(deployArgs, arg)
					}
				}
			}
		}

		containers = append(containers, ContainerConfig{Name: node, OSVersion: value, Privileged: privileged_container, ExposePort: expose_port, Provider: nodeProvider[node], Namespace: namespace, Memory: memory, CPUs: cpus, DeployArgs: deployArgs, Keep: keep})
	}
	// Add docker-image nodes to containers list for network creation
	// (docker-image nodes are stripped from osvers so ConvertStringToMap skips them)
	for node, prov := range nodeProvider {
		if prov == "docker-image" {
			containers = append(containers, ContainerConfig{Name: node, Provider: "docker-image", Namespace: namespace, Keep: keep})
		}
	}
	createNamespace(logger, containers, namespace)
	var nodeIdxs []int
	for k := range nodeDefinitions {
		kStr, _ := strings.CutPrefix(k, "node")
		if nodeNum, err := strconv.Atoi(kStr); err == nil {
			nodeIdxs = append(nodeIdxs, nodeNum)
		}

	}
	sort.Ints(nodeIdxs)
	for _, k := range nodeIdxs {
		nodeName := fmt.Sprintf("node%d", k)
		nodeDef := nodeDefinitions[nodeName]
		deployHost(nodeProvider[nodeName], logger, namespace, nodeName, ansible_hosts_run_file, nodeDef, keep)
	}

	for _, k := range nodeIdxs {
		nodeName := fmt.Sprintf("node%d", k)
		nodeDef := nodeDefinitions[nodeName]
		if nodeProvider[nodeName] == "docker-image" {
			for _, arg := range nodeDef {
				deployment_keyword := ParseDeploymentKeyword(logger, arg)
				if _, ok := deployment_keyword.Args["docker-image"]; ok {
					unmodified_docker.SetupContainer(logger, namespace, nodeName, deployment_keyword.Cmd, deployment_keyword.Args)
				}
			}
		}
	}

	runPlaybook(logger, namespace, ansible_hosts_run_file, verbose)

	corootServers, corootTargets := buildCorootTargets(logger, nodeDefinitions, nodeIdxs)
	unmodified_docker.SetupCoroot(logger, namespace, corootServers, corootTargets)
}

// dbArgDefault reads the default value of a database keyword's argument, so
// coroot-client picks up the same credentials the database was deployed with.
func dbArgDefault(logger *log.Logger, cmd string, subcmd string) string {
	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		return ""
	}
	defer db.Close()
	var value string
	err = db.QueryRow(
		`select arg_default from ansible_arguments where cmd = ? and subcmd = ? limit 1`,
		cmd, subcmd).Scan(&value)
	if err != nil {
		return ""
	}
	return value
}

// buildCorootTargets returns the coroot server nodes in a deploy, and the
// databases to register with them. The database type and credentials come from
// the database keyword deployed next to each "coroot-client", and can be
// overridden with user= and password= on coroot-client itself.
func buildCorootTargets(logger *log.Logger, nodeDefinitions map[string][]string, nodeIdxs []int) ([]string, []unmodified_docker.CorootTarget) {
	servers := []string{}
	targets := []unmodified_docker.CorootTarget{}
	for _, k := range nodeIdxs {
		nodeName := fmt.Sprintf("node%d", k)
		var client *DeploymentKeywordData
		var database *DeploymentKeywordData
		var dbType, dbPort string

		for _, arg := range nodeDefinitions[nodeName] {
			keyword := ParseDeploymentKeyword(logger, arg)
			if keyword.Cmd == "coroot-server" {
				servers = append(servers, nodeName)
				continue
			}
			if keyword.Cmd == "coroot-client" {
				kw := keyword
				client = &kw
				continue
			}
			if t, port := unmodified_docker.CorootInstrumentationType(keyword.Cmd); t != "" {
				kw := keyword
				database = &kw
				dbType, dbPort = t, port
			}
		}

		if client == nil {
			continue
		}
		server, ok := client.Args["server"]
		if !ok {
			logger.Printf("Coroot: %s has coroot-client without server=<node>, skipping", nodeName)
			continue
		}
		if database == nil {
			logger.Printf("Coroot: %s has coroot-client but no database to monitor, skipping", nodeName)
			continue
		}

		target := unmodified_docker.CorootTarget{
			Server:   server,
			Node:     nodeName,
			Type:     dbType,
			Port:     dbPort,
			User:     dbArgDefault(logger, database.Cmd, "user"),
			Password: dbArgDefault(logger, database.Cmd, "password"),
		}
		// A docker-image database is set up by its own entrypoint, not by the
		// ansible role, so it uses the official image's admin user and the
		// shared anydbver password instead of the role defaults.
		if _, isDockerImage := database.Args["docker-image"]; isDockerImage {
			target.Password = anydbver_common.ANYDBVER_DEFAULT_PASSWORD
			if dbType == "mongodb" {
				target.User = "admin"
			}
		}
		if user, ok := database.Args["user"]; ok {
			target.User = user
		}
		if password, ok := database.Args["password"]; ok {
			target.Password = password
		}
		if user, ok := client.Args["user"]; ok {
			target.User = user
		}
		if password, ok := client.Args["password"]; ok {
			target.Password = password
		}
		if port, ok := client.Args["port"]; ok {
			target.Port = port
		}
		targets = append(targets, target)
	}
	return servers, targets
}

func printVersion() {
	fmt.Println("anydbver")
	fmt.Printf("Version %s\n", Version)
	fmt.Printf("Build: %s using %s\n", Build, GoVersion)
	fmt.Printf("Commit: %s\n", Commit)
}

func Contains(slice []string, item string) bool {
	for _, v := range slice {
		if v == item {
			return true
		}
	}
	return false
}

func printAllDeployExamples(logger *log.Logger) {
	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		logger.Printf("failed to open database: %v", err)
		return
	}
	defer db.Close()

	query := `
  SELECT cmd, deploy 
  FROM (
      SELECT DISTINCT 
          keyword.cmd, 
          LTRIM(LTRIM(tests.cmd, '.'), '/') AS deploy
      FROM (
          SELECT DISTINCT cmd, cmd AS alias 
          FROM ansible_arguments 
          UNION 
          SELECT DISTINCT cmd, cmd AS alias 
          FROM k8s_arguments 
          UNION 
          SELECT DISTINCT keyword AS cmd, alias 
          FROM keyword_aliases
      ) AS keyword
      INNER JOIN tests 
          ON tests.cmd LIKE '% ' || keyword.alias || ' %'
          OR tests.cmd LIKE '% ' || keyword.alias || ':%'
      UNION 
      SELECT cmd, deploy 
      FROM help_examples
  ) AS all_help 
  ORDER BY cmd, deploy;
  `

	rows, err := db.Query(query)
	if err != nil {
		logger.Printf("failed to execute select query: %v", err)
		return
	}
	defer rows.Close()

	// Collect the results into a string
	for rows.Next() {
		var keyword string
		var deploy_cmd string
		if err := rows.Scan(&keyword, &deploy_cmd); err != nil {
			logger.Printf("failed to scan row: %v", err)
		}
		fmt.Println(deploy_cmd)
	}
	if err = rows.Err(); err != nil {
		logger.Printf("failed to scan row: %v", err)
	}
}

func printKeywordList(logger *log.Logger) {
	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		logger.Printf("failed to open database: %v", err)
		return
	}
	defer db.Close()

	query := `
  SELECT DISTINCT cmd, 1 ord
  FROM ansible_arguments 
  UNION 
  SELECT DISTINCT cmd, 2 ord
  FROM k8s_arguments 
  ORDER BY ord,cmd;
  `

	rows, err := db.Query(query)
	if err != nil {
		logger.Printf("failed to execute select query: %v", err)
		return
	}
	defer rows.Close()

	// Collect the results into a string
	for rows.Next() {
		var keyword string
		var ord int
		if err := rows.Scan(&keyword, &ord); err != nil {
			logger.Printf("failed to scan row: %v", err)
		}
		fmt.Println(keyword)
	}
	if err = rows.Err(); err != nil {
		logger.Printf("failed to scan row: %v", err)
	}
}

func printKeywordAliases(logger *log.Logger, search_keyword string) {
	fmt.Println("Aliases for command(software)", search_keyword)
	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		logger.Printf("failed to open database: %v", err)
		return
	}
	defer db.Close()

	query := `SELECT alias FROM keyword_aliases WHERE keyword = ?`

	rows, err := db.Query(query, search_keyword)
	if err != nil {
		logger.Printf("failed to execute select query: %v", err)
		return
	}
	defer rows.Close()

	// Collect the results into a string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			logger.Printf("failed to scan row: %v", err)
		}
		fmt.Println(alias)
	}
	if err = rows.Err(); err != nil {
		logger.Printf("failed to scan row: %v", err)
	}
	fmt.Println("")
}

func printKeywordSubCommands(logger *log.Logger, search_keyword string) {
	fmt.Println("Subcommands (parameters) for command", search_keyword)
	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		logger.Printf("failed to open database: %v", err)
		return
	}
	defer db.Close()

	query := `select distinct subcmd from ansible_arguments where cmd = ? UNION select distinct subcmd from k8s_arguments where cmd = ?`

	rows, err := db.Query(query, search_keyword, search_keyword)
	if err != nil {
		logger.Printf("failed to execute select query: %v", err)
		return
	}
	defer rows.Close()

	// Collect the results into a string
	for rows.Next() {
		var alias string
		if err := rows.Scan(&alias); err != nil {
			logger.Printf("failed to scan row: %v", err)
		}
		fmt.Println(alias)
	}
	if err = rows.Err(); err != nil {
		logger.Printf("failed to scan row: %v", err)
	}
	fmt.Println("")
}

func printOneDeployCommandExamples(logger *log.Logger, args []string) {
	fmt.Println(args)
	var filter_commands []string
	search_keyword := ""
	for _, arg := range args {
		deployment_keyword := ParseDeploymentKeyword(logger, arg)
		fmt.Println(deployment_keyword.Cmd)
		filter_commands = append(filter_commands, deployment_keyword.Cmd)
		search_keyword = deployment_keyword.Cmd
		for subcmd := range deployment_keyword.Args {
			if subcmd != "version" && subcmd != "help" {
				fmt.Println(subcmd)
			}
		}
	}

	if search_keyword == "" {
		logger.Println("There is no keywords in ", args)
		return
	}
	if search_keyword == "keywords" {
		printKeywordList(logger)
		return
	}

	printKeywordAliases(logger, search_keyword)
	printKeywordSubCommands(logger, search_keyword)

	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		logger.Printf("failed to open database: %v", err)
		return
	}
	defer db.Close()

	query := `
  SELECT cmd, deploy 
  FROM (
      SELECT DISTINCT 
          keyword.cmd, 
          LTRIM(LTRIM(tests.cmd, '.'), '/') AS deploy
      FROM (
          SELECT DISTINCT cmd, cmd AS alias 
          FROM ansible_arguments 
          UNION 
          SELECT DISTINCT cmd, cmd AS alias 
          FROM k8s_arguments 
          UNION 
          SELECT DISTINCT keyword AS cmd, alias 
          FROM keyword_aliases
      ) AS keyword
      INNER JOIN tests 
          ON tests.cmd LIKE '% ' || keyword.alias || ' %'
          OR tests.cmd LIKE '% ' || keyword.alias || ':%'
      UNION 
      SELECT cmd, deploy 
      FROM help_examples
  ) AS all_help
  WHERE cmd = ?
  ORDER BY cmd, deploy;
  `

	rows, err := db.Query(query, search_keyword)
	if err != nil {
		logger.Printf("failed to execute select query: %v", err)
		return
	}
	defer rows.Close()

	// Collect the results into a string
	for rows.Next() {
		var keyword string
		var deploy_cmd string
		if err := rows.Scan(&keyword, &deploy_cmd); err != nil {
			logger.Printf("failed to scan row: %v", err)
		}
		if Contains(filter_commands, keyword) {
			fmt.Println(deploy_cmd)
		}
	}
	if err = rows.Err(); err != nil {
		logger.Printf("failed to scan row: %v", err)
	}
}

func helpDeployCommands(logger *log.Logger, provider string, args []string) {
	fmt.Println("Help on deployment commands:")
	fmt.Println("anydbver help           # shows a full list of examples")
	fmt.Println("anydbver help [keyword] # shows examples for specific keyword(software)")
	fmt.Println("anydbver help keywords  # shows a list of keywords(software)")
	all_commands := false
	if len(args) == 1 && args[0] == "help" {
		logger.Println("Help for all deployment keywords")
		all_commands = true
	}
	if all_commands {
		printAllDeployExamples(logger)
	} else {
		printOneDeployCommandExamples(logger, args)
	}
}

func main() {
	var provider string
	var namespace string
	var memory string
	var cpus string
	var verbose bool

	if Version == "unknown" {
		Version = version
		Commit = commit
		Build = date
	}

	logger := log.New(os.Stdout, "", log.LstdFlags)

	rootCmd := &cobra.Command{
		Use:   "anydbver",
		Short: "A tool for database environments automation",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			if provider == "" {
				provider = "docker"
			}

			dbFile := anydbver_common.GetDatabasePath(logger)
			binVer := strings.TrimPrefix(Version, "v")
			// Only nag when the local version DB is genuinely older than this
			// binary. Skip dev/unknown builds and unreadable/empty versions so
			// we don't warn forever on every command (see versionLess).
			if dbVer, err := ReadDatabaseVersion(dbFile); err == nil && dbVer != "" &&
				binVer != "dev" && binVer != "unknown" && versionLess(dbVer, binVer) {
				logger.Println("Version database update is available for", dbFile, ". Run anydbver update to see latest versions")
			}
		},
		Version: fmt.Sprintf("\nVersion: %s\nBuild: %s using %s\nCommit: %s", Version, Build, GoVersion, Commit),
	}
	namespaceCmd := &cobra.Command{
		Use:   "namespace",
		Short: "Manage namespaces",
	}
	nsCreateCmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a namespace with containers",
		Args:  cobra.ExactArgs(1), // Expect exactly one positional argument (name)
		Run: func(cmd *cobra.Command, args []string) {
			namespace := args[0]
			osvers, _ := cmd.Flags().GetString("os")
			expose_ports_str, _ := cmd.Flags().GetString("expose")
			privileged, _ := cmd.Flags().GetString("privileged")

			containers := []ContainerConfig{}
			priv_map := ConvertStringToMap(privileged)
			expose_ports := ConvertStringToMap(expose_ports_str)
			provider := "docker"
			for node, value := range ConvertStringToMap(osvers) {
				privileged_container := true
				if val, ok := priv_map[node]; ok {
					if priv, err := strconv.ParseBool(val); err == nil {
						privileged_container = priv
					}
				}
				expose_port := ""
				if ep, ok := expose_ports[node]; ok {
					expose_port = ep
				}

				containers = append(containers, ContainerConfig{Name: node, OSVersion: value, Privileged: privileged_container, ExposePort: expose_port, Provider: provider, Namespace: namespace})
			}
			createNamespace(logger, containers, namespace)
		},
	}
	listNsCmd := &cobra.Command{
		Use:   "list",
		Short: "List namespaces",
		Run: func(cmd *cobra.Command, args []string) {
			listNamespaces(provider, logger)
		},
	}
	deleteNsCmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete namespace",
		Args:  cobra.ExactArgs(1), // Expect exactly one positional argument (name)
		Run: func(cmd *cobra.Command, args []string) {
			deleteNamespace(logger, provider, args[0])
		},
	}
	destroyCmd := &cobra.Command{
		Use:   "destroy",
		Short: "Delete containers and clusters for current namespace",
		Run: func(cmd *cobra.Command, args []string) {
			deleteNamespace(logger, provider, namespace)
			removeCache, _ := cmd.Flags().GetBool("remove-cache")
			if removeCache {
				removeCacheImages(logger)
			}
		},
	}
	destroyCmd.Flags().BoolP("remove-cache", "", false, "Also remove anydbver-cache images")
	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Deletes current version information database and downloads latest one from https://github.com/zelmario/anydbver/blob/master/anydbver_version.sql",
		Run: func(cmd *cobra.Command, args []string) {
			dbFile := anydbver_common.GetDatabasePath(logger)
			if len(args) >= 1 {
				if err := versionfetch.VersionFetch(args[0], dbFile); err != nil {
					logger.Println("Error fetching versions", err)
				}
			} else {
				os.Remove(dbFile)
				anydbver_common.UpdateSqliteDatabase(logger, dbFile)
			}
		},
	}
	playbookCmd := &cobra.Command{
		Use:   "playbook",
		Short: "Run ansible playbook",
		Run: func(cmd *cobra.Command, args []string) {
			runPlaybook(logger, namespace, anydbver_common.GetAnsibleInventory(logger, namespace), verbose)
		},
	}

	testCmd := &cobra.Command{
		Use:   "test",
		Short: "Run tests",
		Args:  cobra.ExactArgs(1), // Expect exactly one positional argument (name)
		Run: func(cmd *cobra.Command, args []string) {
			skip_os, _ := cmd.Flags().GetString("skip-os")
			var skip_os_list []string
			if skip_os != "" {
				skip_os_list = strings.Split(skip_os, ",")
			}
			registry_cache, _ := cmd.Flags().GetString("registry-cache")
			testAnydbver(logger, provider, namespace, args[0], skip_os_list, registry_cache)
		},
	}
	testCmd.Flags().StringP("skip-os", "", "", "Skip tests with specific OS")
	testCmd.Flags().StringP("registry-cache", "", "", "Add a docker registry mirror to all k3d calls")

	nsCreateCmd.Flags().StringP("os", "o", "", "Operating system of containers: node0=osver,node1=osver...")
	nsCreateCmd.Flags().StringP("privileged", "", "", "Whether the container should be privileged: node0=true,node1=true...")

	namespaceCmd.AddCommand(nsCreateCmd)
	namespaceCmd.AddCommand(listNsCmd)
	namespaceCmd.AddCommand(deleteNsCmd)
	rootCmd.AddCommand(namespaceCmd)
	rootCmd.AddCommand(destroyCmd)
	rootCmd.AddCommand(updateCmd)
	rootCmd.AddCommand(testCmd)
	rootCmd.AddCommand(playbookCmd)

	containerCmd := &cobra.Command{
		Use:   "container",
		Short: "Manage containers",
	}
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List containers",
		Run: func(cmd *cobra.Command, args []string) {
			listContainers(logger, provider, namespace)
		},
	}
	createCmd := &cobra.Command{
		Use:   "create [name]",
		Short: "Create a container",
		Args:  cobra.ExactArgs(1), // Expect exactly one positional argument (name)
		Run: func(cmd *cobra.Command, args []string) {
			name := args[0]
			os, _ := cmd.Flags().GetString("os")
			expose_port, _ := cmd.Flags().GetString("expose")
			privileged, _ := cmd.Flags().GetBool("privileged")
			createContainer(logger, ContainerConfig{Name: name, OSVersion: os, Privileged: privileged, ExposePort: expose_port, Provider: provider, Namespace: namespace, DeployArgs: []string{}})
		},
	}

	createCmd.Flags().StringP("os", "o", "", "Operating system of the container")
	createCmd.Flags().StringP("expose", "p", "", "Expose port, docker -p")
	createCmd.Flags().BoolP("privileged", "", true, "Whether the container should be privileged")

	deployCmd := &cobra.Command{
		Use:   "deploy",
		Short: "deploy hosts",
		Run: func(cmd *cobra.Command, args []string) {
			keep, _ := cmd.Flags().GetBool("keep")
			if len(args) > 0 && args[0] == "help" {
				helpDeployCommands(logger, provider, args)
				os.Exit(0)
			}

			// Reject unknown keywords before touching anything - in particular
			// before the implicit destroy below, so a typo can't wipe an
			// existing environment.
			validateDeployKeywords(logger, args)

			if !keep {
				deleteNamespace(logger, provider, namespace)
			}
			deployHosts(logger, anydbver_common.GetAnsibleInventory(logger, namespace), provider, namespace, args, verbose, memory, cpus, keep)
		},
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) != 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return fetchDeployCompletions(logger, toComplete), cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
		},
	}
	deployCmd.Flags().BoolP("keep", "", false, "do not remove existing containers and network")

	// deployCmd.ValidArgsFunction = cobra.FixedCompletions(fetchDeployCompletions(logger), cobra.ShellCompDirectiveNoSpace)
	rootCmd.AddCommand(deployCmd)

	execCmd := &cobra.Command{
		Use:   "exec",
		Short: "exec command in the container",
		Run: func(cmd *cobra.Command, args []string) {
			containerExec(logger, provider, namespace, args)
		},
	}

	rootCmd.AddCommand(execCmd)

	versionsCmd := &cobra.Command{
		Use:   "versions [software]",
		Short: "List software versions anydbver can deploy",
		Long: "List the versions anydbver can deploy.\n\n" +
			"  anydbver versions            # overview of every deployable software\n" +
			"  anydbver versions pg         # all PostgreSQL versions, grouped by major\n" +
			"  anydbver versions pg --latest# newest version per major series\n" +
			"  anydbver versions psmdb --os el9\n" +
			"  anydbver versions ps --all   # raw package versions with os/arch\n" +
			"  anydbver versions --json     # machine-readable",
		Run: func(cmd *cobra.Command, args []string) {
			osFilter, _ := cmd.Flags().GetString("os")
			archFilter, _ := cmd.Flags().GetString("arch")
			latest, _ := cmd.Flags().GetBool("latest")
			all, _ := cmd.Flags().GetBool("all")
			asJSON, _ := cmd.Flags().GetBool("json")
			runVersions(logger, anydbver_common.GetDatabasePath(logger), args, osFilter, archFilter, latest, all, asJSON)
		},
	}
	versionsCmd.Flags().StringP("os", "o", "", "Only versions available for this OS (e.g. el9, jammy)")
	versionsCmd.Flags().StringP("arch", "a", "", "Only versions available for this arch (x86_64, aarch64)")
	versionsCmd.Flags().Bool("latest", false, "Show only the newest version per major series")
	versionsCmd.Flags().Bool("all", false, "Show raw package versions with os/arch availability")
	versionsCmd.Flags().Bool("json", false, "Output as JSON")
	rootCmd.AddCommand(versionsCmd)

	shellCmd := &cobra.Command{
		Use:   "shell",
		Short: "Start a shell with ansible and kubectl",
		Run: func(cmd *cobra.Command, args []string) {
			shellExec(logger, provider, namespace, args)
		},
	}

	rootCmd.AddCommand(shellCmd)

	everestCmd := &cobra.Command{
		Use:   "everest",
		Short: "Deploy Percona Everest on k3d cluster",
		Long: `Deploy a k3d Kubernetes cluster with Percona Everest installed.
This provides a local Database-as-a-Service platform for MySQL, PostgreSQL, and MongoDB.`,
		Run: func(cmd *cobra.Command, args []string) {
			nodes, _ := cmd.Flags().GetInt("nodes")
			everestVersion, _ := cmd.Flags().GetString("version")

			deleteNamespace(logger, provider, namespace)
			deployEverest(logger, namespace, nodes, everestVersion)
		},
	}
	everestCmd.Flags().Int("nodes", 3, "Number of k3d nodes")
	everestCmd.Flags().String("version", "latest", "Percona Everest version")

	rootCmd.AddCommand(everestCmd)

	chaosCmd := &cobra.Command{
		Use:    "chaos",
		Short:  "Inject network faults into deployed containers (experimental)",
		Hidden: true,
	}
	chaosLinkCmd := &cobra.Command{
		Use:   "link [nodeA] [nodeB] [delay=100ms] [jitter=20ms] [loss=5%]",
		Short: "Degrade the network link between two nodes (symmetric)",
		Args:  cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			params, err := chaosParseParams(args[2:])
			if err != nil {
				logger.Fatalf("chaos: %v", err)
			}
			ttl, _ := cmd.Flags().GetInt("ttl")
			if err := chaosDegradeLink(logger, provider, namespace, args[0], args[1], params, ttl); err != nil {
				logger.Fatalf("chaos: %v", err)
			}
		},
	}
	chaosLinkCmd.Flags().Int("ttl", chaosDefaultTTLSec, "Seconds before faults auto-clear (0 to disable)")
	chaosPartitionCmd := &cobra.Command{
		Use:   "partition [nodeA] [nodeB]",
		Short: "Fully sever the link between two nodes (100% packet loss both ways)",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			ttl, _ := cmd.Flags().GetInt("ttl")
			if err := chaosPartition(logger, provider, namespace, args[0], args[1], ttl); err != nil {
				logger.Fatalf("chaos: %v", err)
			}
		},
	}
	chaosPartitionCmd.Flags().Int("ttl", chaosDefaultTTLSec, "Seconds before the partition auto-heals (0 to disable)")
	newChaosNodeCmd := func(action, short string) *cobra.Command {
		return &cobra.Command{
			Use:   action + " [node]",
			Short: short,
			Args:  cobra.ExactArgs(1),
			Run: func(cmd *cobra.Command, args []string) {
				if err := chaosNodeAction(logger, provider, namespace, args[0], action); err != nil {
					logger.Fatalf("chaos: %v", err)
				}
			},
		}
	}
	chaosUICmd := &cobra.Command{
		Use:   "ui",
		Short: "Launch the interactive chaos dashboard in a browser",
		Run: func(cmd *cobra.Command, args []string) {
			port, _ := cmd.Flags().GetInt("port")
			ttl, _ := cmd.Flags().GetInt("ttl")
			flux, _ := cmd.Flags().GetInt("flux")
			runChaosUI(logger, provider, namespace, port, ttl, flux)
		},
	}
	chaosUICmd.Flags().Int("port", 8080, "Port for the dashboard HTTP server")
	chaosUICmd.Flags().Int("ttl", chaosDefaultTTLSec, "Seconds before faults auto-clear if the dashboard exits unexpectedly (0 to disable)")
	chaosUICmd.Flags().Int("flux", 0, "Re-roll ranged params every N seconds (0 = off; can also toggle in the UI)")
	chaosClearCmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove all injected network faults in the namespace",
		Run: func(cmd *cobra.Command, args []string) {
			if err := chaosClearAll(logger, provider, namespace); err != nil {
				logger.Fatalf("chaos: %v", err)
			}
		},
	}
	chaosStatusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show current network shaping per node",
		Run: func(cmd *cobra.Command, args []string) {
			if err := chaosStatus(logger, provider, namespace); err != nil {
				logger.Fatalf("chaos: %v", err)
			}
		},
	}
	chaosMeasureCmd := &cobra.Command{
		Use:   "measure [nodeA] [nodeB]",
		Short: "Measure actual latency between two nodes (RTT + one-way ≈ RTT/2)",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if err := chaosMeasure(logger, provider, namespace, args[0], args[1]); err != nil {
				logger.Fatalf("chaos: %v", err)
			}
		},
	}
	chaosCmd.AddCommand(chaosLinkCmd)
	chaosCmd.AddCommand(chaosPartitionCmd)
	chaosCmd.AddCommand(chaosMeasureCmd)
	chaosCmd.AddCommand(newChaosNodeCmd("pause", "Freeze a node (docker pause)"))
	chaosCmd.AddCommand(newChaosNodeCmd("unpause", "Unfreeze a node (docker unpause)"))
	chaosCmd.AddCommand(newChaosNodeCmd("kill", "Hard-stop a node (docker kill)"))
	chaosCmd.AddCommand(newChaosNodeCmd("start", "Restart a stopped node (docker start)"))
	chaosCmd.AddCommand(chaosUICmd)
	chaosCmd.AddCommand(chaosClearCmd)
	chaosCmd.AddCommand(chaosStatusCmd)
	rootCmd.AddCommand(chaosCmd)

	rootCmd.PersistentFlags().StringVarP(&provider, "provider", "p", "", "Container provider")
	rootCmd.PersistentFlags().StringVarP(&namespace, "namespace", "n", "", "Namespace")
	rootCmd.PersistentFlags().StringVarP(&memory, "memory", "m", "", "Default memory amount per node")
	rootCmd.PersistentFlags().StringVarP(&cpus, "cpus", "", "", "Default number of CPU core per node")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "", false, "Verbose ansible output")

	rootCmd.AddCommand(listCmd)

	containerCmd.AddCommand(listCmd)
	containerCmd.AddCommand(createCmd)
	rootCmd.AddCommand(containerCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
