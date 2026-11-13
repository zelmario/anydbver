package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

// Every EL node used to download and parse the BaseOS, AppStream, extras and
// EPEL metadata on its own: about 48 s of wall time and 32 s of CPU per node.
// With 10 nodes on 8 cores that alone took 7 minutes and pushed sharded
// clusters past the ansible timeout. Now anydbver runs "dnf makecache" once per
// OS and architecture into a host directory, and each node copies the result,
// including the parsed .solv files, into its own /var/cache/dnf.
//
// The seed is reused for dnfSeedTTL. dnf itself trusts metadata for 48 hours,
// but EPEL removes superseded packages daily, so a day-old seed can point at
// RPMs that are gone. Set ANYDBVER_NO_DNF_SEED=1 to turn the seed off.

const (
	dnfSeedTTL       = 6 * time.Hour
	dnfSeedMountPath = "/var/cache/anydbver-dnf-seed"
	dnfSeedStamp     = ".anydbver-seed"
)

var dnfSeedOS = map[string]bool{"el8": true, "el9": true, "el10": true}

func dnfSeedEnabled() bool {
	return os.Getenv("ANYDBVER_NO_DNF_SEED") == ""
}

func dnfSeedRoot(logger *log.Logger) string {
	return filepath.Join(anydbver_common.GetCacheDirectory(logger), "data", "dnf-seed")
}

// dnfSeedArch matches ansible_architecture, which the playbook uses to find
// the seed.
func dnfSeedArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

func dnfSeedFresh(logger *log.Logger, key string) bool {
	fi, err := os.Stat(filepath.Join(dnfSeedRoot(logger), key, dnfSeedStamp))
	return err == nil && time.Since(fi.ModTime()) < dnfSeedTTL
}

// refreshDnfSeed runs makecache in a throwaway container from the node image.
// Everything happens inside the container because the files are owned by
// root: the new seed is built in a temporary directory and renamed into place,
// so a node never copies a half-written cache.
func refreshDnfSeed(logger *log.Logger, osver string, user string) error {
	key := osver + "-" + dnfSeedArch()
	script := fmt.Sprintf(`set -e
cd /seed
tmp=%[1]s.tmp.$(hostname)
rm -rf "$tmp"
dnf -q makecache >/dev/null
mkdir -p "$tmp"
cp -a /var/cache/dnf/. "$tmp"/
touch "$tmp"/%[2]s
rm -rf %[1]s.old
if [ -d %[1]s ]; then mv %[1]s %[1]s.old; fi
mv "$tmp" %[1]s
rm -rf %[1]s.old`, key, dnfSeedStamp)

	args := []string{
		"docker", "run", "--rm",
		"--platform", "linux/" + runtime.GOARCH,
		"-v", dnfSeedRoot(logger) + ":/seed:z",
		anydbver_common.GetDockerImageName(osver, user),
		"bash", "-c", script,
	}
	_, err := runtools.RunPipe(logger, args, "Error refreshing dnf metadata", regexp.MustCompile("ignore this"), false, map[string]string{}, 600)
	return err
}

// seedDnfMetadata refreshes the seed for every EL version in the deployment
// that does not have a fresh one. Failures only cost speed: the nodes fall
// back to fetching metadata themselves.
func seedDnfMetadata(logger *log.Logger, containers []ContainerConfig) {
	if !dnfSeedEnabled() {
		return
	}
	user := anydbver_common.GetUser(logger)
	todo := map[string]bool{}
	for _, c := range containers {
		if c.Provider == "docker" && dnfSeedOS[c.OSVersion] && !dnfSeedFresh(logger, c.OSVersion+"-"+dnfSeedArch()) {
			todo[c.OSVersion] = true
		}
	}
	if len(todo) == 0 {
		return
	}
	os.MkdirAll(dnfSeedRoot(logger), 0755)

	var wg sync.WaitGroup
	for osver := range todo {
		wg.Add(1)
		go func(osver string) {
			defer wg.Done()
			fmt.Printf("Refreshing dnf metadata for %s once, all %s nodes reuse it for %s...\n", osver, osver, dnfSeedTTL)
			start := time.Now()
			if err := refreshDnfSeed(logger, osver, user); err != nil {
				logger.Printf("Warning: could not refresh the dnf metadata seed for %s, each node fetches its own: %v\n", osver, err)
				return
			}
			fmt.Printf("dnf metadata for %s ready in %s\n", osver, time.Since(start).Round(time.Second))
		}(osver)
	}
	wg.Wait()
}

// dnfSeedVolume is the docker run argument that gives a node read-only access
// to all seeds. The parent directory is mounted, not a single seed, so a node
// copies whatever seed is current when its playbook starts.
func dnfSeedVolume(logger *log.Logger, osver string) []string {
	if !dnfSeedEnabled() || !dnfSeedOS[osver] {
		return nil
	}
	return []string{"-v", dnfSeedRoot(logger) + ":" + dnfSeedMountPath + ":ro,z"}
}
