package main

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

// pgTdeMinMajor is the first Percona Distribution for PostgreSQL major with
// pg_tde packages.
const pgTdeMinMajor = 17

// validatePgTde rejects pg-tde deployments that would fail half way through
// the playbook: pg_tde is a Percona Distribution for PostgreSQL extension, it
// needs a Vault to keep its keys in, and a streaming replica of an encrypted
// primary cannot start without the extension and the Vault token.
//
// It also pins both versions: pg_tde only loads into the server builds it was
// made for, so it picks the newest compatible pair from pg_tde_compat and
// returns args with exact versions on the ppg and pg-tde items.
func validatePgTde(logger *log.Logger, args []string) []string {
	type nodeInfo struct {
		ppgVersion string
		ppgIdx     int
		hasPpg     bool
		hasPgTde   bool
		pgTdeArg   string
		pgTdeIdx   int
		tdeVersion string
		master     string
	}
	nodes := map[string]*nodeInfo{}
	order := []string{}
	current := "node0"
	get := func(name string) *nodeInfo {
		if n, ok := nodes[name]; ok {
			return n
		}
		n := &nodeInfo{}
		nodes[name] = n
		order = append(order, name)
		return n
	}
	get(current)

	failed := false
	fail := func(format string, v ...interface{}) {
		logger.Printf("Error: "+format, v...)
		failed = true
	}

	for i, arg := range args {
		if strings.HasPrefix(arg, "node") && !strings.ContainsAny(arg, ":,=") {
			current = arg
			get(current)
			continue
		}
		if arg == "default" {
			current = "node0"
			continue
		}
		// Options only follow the version slot, after a colon. Without one the
		// whole item becomes the keyword and the options are dropped.
		if bare := strings.SplitN(arg, ",", 2)[0]; (bare == "pg-tde" || bare == "pg_tde") && strings.Contains(arg, ",") {
			fail("options of pg-tde go after a colon: write %q, not %q.", bare+":"+strings.SplitN(arg, ",", 2)[1], arg)
			continue
		}
		keyword := ParseDeploymentKeyword(logger, arg)
		n := get(current)
		switch keyword.Cmd {
		case "percona-postgresql":
			n.hasPpg = true
			n.ppgIdx = i
			n.ppgVersion = keyword.Args["version"]
			if m, ok := keyword.Args["master"]; ok {
				n.master = m
			} else if m, ok := keyword.Args["primary"]; ok {
				n.master = m
			}
		case "postgresql":
			if _, ok := keyword.Args["docker-image"]; !ok {
				// Remember it to give a better message below.
				n.ppgVersion = "pgdg"
			}
		case "pg-tde":
			n.hasPgTde = true
			n.pgTdeArg = arg
			n.pgTdeIdx = i
			n.tdeVersion = keyword.Args["version"]
			if vault, ok := keyword.Args["vault"]; !ok || vault == "" {
				fail("pg-tde needs a Vault server for its keys (from %q).", arg)
				logger.Printf("  Deploy one with the vault keyword and point at it, e.g. 'anydbver deploy vault node1 ppg:17 pg-tde:vault=node0'")
			}
		}
	}

	for _, name := range order {
		n := nodes[name]
		if !n.hasPgTde {
			continue
		}
		if !n.hasPpg {
			if n.ppgVersion == "pgdg" {
				fail("pg-tde needs Percona Distribution for PostgreSQL, %s deploys community PostgreSQL (from %q).", name, n.pgTdeArg)
				logger.Printf("  pg_tde relies on server changes that only Percona's build has. Use ppg:17 or ppg:18 instead of pg.")
			} else {
				fail("pg-tde on %s needs ppg on the same node (from %q).", name, n.pgTdeArg)
				logger.Printf("  e.g. '%s ppg:17 pg-tde:vault=node0'", name)
			}
			continue
		}
		if major, ok := pgMajor(n.ppgVersion); ok && major < pgTdeMinMajor {
			fail("pg-tde needs Percona Distribution for PostgreSQL %d or newer, %s deploys ppg:%s.", pgTdeMinMajor, name, n.ppgVersion)
		}
	}

	// A replica copies the primary's data files, encrypted tables included,
	// and replays its WAL, so it needs the extension loaded and the token too.
	for _, name := range order {
		n := nodes[name]
		if n.master == "" || n.hasPgTde {
			continue
		}
		master := n.master
		if master == "default" {
			master = "node0"
		}
		if p, ok := nodes[master]; ok && p.hasPgTde {
			fail("%s replicates from %s, which has pg-tde, but %s has no pg-tde item.", name, master, name)
			logger.Printf("  Add the same pg-tde item to %s, e.g. '%s'.", name, p.pgTdeArg)
		}
	}

	if failed {
		os.Exit(runtools.ANYDBVER_UNKNOWN_OPTION)
	}

	out := append([]string{}, args...)
	db, err := sql.Open("sqlite", anydbver_common.GetDatabasePath(logger))
	if err != nil {
		return out
	}
	defer db.Close()
	for _, name := range order {
		n := nodes[name]
		if !n.hasPgTde {
			continue
		}
		major, ok := pgMajor(n.ppgVersion)
		if !ok {
			// Unpinned ppg is the newest series that has pg_tde builds.
			if db.QueryRow(`SELECT max(CAST(major AS integer)) FROM pg_tde_compat`).Scan(&major) != nil {
				continue
			}
		}
		server, tde, err := pickPgTdePair(db, major, n.ppgVersion, n.tdeVersion)
		if err != nil {
			fail("%s: %v", name, err)
			continue
		}
		if server == "" {
			want := fmt.Sprintf("ppg:%s", n.ppgVersion)
			if n.ppgVersion == "" || n.ppgVersion == "latest" {
				want = fmt.Sprintf("ppg %d", major)
			}
			fail("%s: no %s build works with pg-tde %s (from %q).", name, want, orLatest(n.tdeVersion), n.pgTdeArg)
			logger.Printf("  pg_tde only loads into the server builds it was made for. Pairs anydbver knows for %d:", major)
			for _, pair := range pgTdePairs(db, major) {
				logger.Printf("    ppg:%s pg-tde:%s", pair[0], pair[1])
			}
			continue
		}
		if n.ppgVersion != server {
			logger.Printf("%s: pg-tde %s needs a matching server build, deploying ppg:%s", name, tde, server)
		}
		out[n.ppgIdx] = setItemVersion(out[n.ppgIdx], server)
		out[n.pgTdeIdx] = setItemVersion(out[n.pgTdeIdx], tde)
	}
	if failed {
		os.Exit(runtools.ANYDBVER_UNKNOWN_VERSION)
	}
	return out
}

// versionMatches says whether a full version ("17.10-2", "2.2.1-2") is what a
// user asked for: empty and "latest" match anything, otherwise an exact match
// or a prefix ending at a "." or "-" boundary, so "17.1" does not match
// "17.10-1" but "17.10" and "2.2" do.
func versionMatches(full, want string) bool {
	if want == "" || want == "latest" || full == want {
		return true
	}
	return strings.HasPrefix(full, want+".") || strings.HasPrefix(full, want+"-")
}

// pickPgTdePair returns the newest server build and pg_tde package of a major
// that work together and match what was asked for, or empty strings.
func pickPgTdePair(db *sql.DB, major int, ppgWant, tdeWant string) (string, string, error) {
	best := [2]string{}
	for _, pair := range pgTdePairs(db, major) {
		if !versionMatches(pair[0], ppgWant) || !versionMatches(pair[1], tdeWant) {
			continue
		}
		if best[0] == "" || buildLess(best[0], pair[0]) ||
			(best[0] == pair[0] && buildLess(best[1], pair[1])) {
			best = pair
		}
	}
	return best[0], best[1], nil
}

// pgTdePairs lists the (server, pg_tde) builds known to work for a major.
func pgTdePairs(db *sql.DB, major int) [][2]string {
	rows, err := db.Query(`SELECT ppg_version, pg_tde_version FROM pg_tde_compat WHERE major = ?`, strconv.Itoa(major))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var pairs [][2]string
	for rows.Next() {
		var p [2]string
		if rows.Scan(&p[0], &p[1]) == nil {
			pairs = append(pairs, p)
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i][0] != pairs[j][0] {
			return buildLess(pairs[i][0], pairs[j][0])
		}
		return buildLess(pairs[i][1], pairs[j][1])
	})
	return pairs
}

// setItemVersion replaces, or adds, the version of a deploy item:
// "ppg:17,master=node1" -> "ppg:17.11-1,master=node1",
// "pg-tde:vault=node0" -> "pg-tde:2.2.2-1,vault=node0".
func setItemVersion(arg, version string) string {
	parts := strings.SplitN(arg, ":", 2)
	if len(parts) == 1 {
		return parts[0] + ":" + version
	}
	opts := strings.Split(parts[1], ",")
	if (IsDeploymentVersion(opts[0]) || opts[0] == "latest") && !strings.Contains(opts[0], "=") {
		opts[0] = version
	} else {
		opts = append([]string{version}, opts...)
	}
	return parts[0] + ":" + strings.Join(opts, ",")
}

// buildLess compares "17.10-1" style versions, the package release after the
// "-" included, which versionLess ignores.
func buildLess(a, b string) bool {
	if versionLess(a, b) || versionLess(b, a) {
		return versionLess(a, b)
	}
	release := func(v string) int {
		if i := strings.Index(v, "-"); i >= 0 {
			n, _ := strconv.Atoi(v[i+1:])
			return n
		}
		return 0
	}
	return release(a) < release(b)
}

func orLatest(v string) string {
	if v == "" || v == "latest" {
		return "(any version)"
	}
	return v
}

// pgMajor returns the major version of a ppg version argument: 17 for "17",
// "17.6" or "17.6-1". "latest" and empty versions report false, they resolve to
// the newest series.
func pgMajor(version string) (int, bool) {
	if version == "" || version == "latest" {
		return 0, false
	}
	major := strings.SplitN(strings.SplitN(version, "-", 2)[0], ".", 2)[0]
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, false
	}
	return n, true
}
