package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// versionSource describes one deployable software and where its versions live.
// Loaded from the version_sources table (see anydbver_version.sql) so new
// software shows up in `anydbver versions` without code changes.
type versionSource struct {
	Keyword     string
	DisplayName string
	SourceTable string
	Program     string // general_version.program / k8s_operators_version.name filter; "" otherwise
	StripBuild  bool   // group to upstream version (drop the -build suffix) when true
	OrderNo     int
}

// allowedVersionTables guards the table name we interpolate into queries.
// source_table comes from our own DB, but we still whitelist to be safe.
var allowedVersionTables = map[string]bool{
	"postgresql_version":             true,
	"percona_postgresql_version":     true,
	"percona_server_version":         true,
	"mysql_server_version":           true,
	"mariadb_version":                true,
	"percona_xtradb_cluster_version": true,
	"mydb_version":                   true,
	"percona_server_mongodb_version": true,
	"percona_xtrabackup_version":     true,
	"percona_backup_mongodb_version": true,
	"general_version":                true,
	"k8s_operators_version":          true,
	"docker_hub":                     true, // not a table: tags fetched from Docker Hub (program = repo)
}

func loadVersionSources(dbFile string) ([]versionSource, error) {
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT keyword, display_name, source_table, program, strip_build, orderno
		FROM version_sources ORDER BY orderno`)
	if err != nil {
		return nil, fmt.Errorf("failed to query version_sources: %w", err)
	}
	defer rows.Close()

	var out []versionSource
	for rows.Next() {
		var s versionSource
		var strip int
		if err := rows.Scan(&s.Keyword, &s.DisplayName, &s.SourceTable, &s.Program, &strip, &s.OrderNo); err != nil {
			return nil, fmt.Errorf("failed to scan version_sources row: %w", err)
		}
		s.StripBuild = strip != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// versionRow is one (version, os, arch) availability tuple. os/arch are "" for
// sources that have no such columns (k8s_operators_version).
type versionRow struct {
	version string
	os      string
	arch    string
}

func queryVersionRows(dbFile string, s versionSource) ([]versionRow, error) {
	if !allowedVersionTables[s.SourceTable] {
		return nil, fmt.Errorf("unknown version source table %q", s.SourceTable)
	}
	if s.SourceTable == "docker_hub" {
		return fetchDockerHubTags(s.Program)
	}

	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	defer db.Close()

	var query string
	var qargs []interface{}
	switch s.SourceTable {
	case "k8s_operators_version":
		query = `SELECT version, '', '' FROM k8s_operators_version WHERE name = ?`
		qargs = append(qargs, s.Program)
	case "general_version":
		query = `SELECT version, os, arch FROM general_version WHERE program = ?`
		qargs = append(qargs, s.Program)
	default:
		query = `SELECT version, os, arch FROM ` + s.SourceTable
	}

	rows, err := db.Query(query, qargs...)
	if err != nil {
		return nil, fmt.Errorf("failed to query %s: %w", s.SourceTable, err)
	}
	defer rows.Close()

	var out []versionRow
	for rows.Next() {
		var r versionRow
		if err := rows.Scan(&r.version, &r.os, &r.arch); err != nil {
			return nil, fmt.Errorf("failed to scan version row: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// fetchDockerHubTags lists version-like tags for a Docker Hub repo (e.g.
// percona/pmm-server), which has no entries in the version DB. Network is
// required; callers treat an error as "unavailable" rather than fatal.
func fetchDockerHubTags(repo string) ([]versionRow, error) {
	client := &http.Client{Timeout: 8 * time.Second}
	url := fmt.Sprintf("https://hub.docker.com/v2/repositories/%s/tags/?page_size=100&ordering=last_updated", repo)

	var out []versionRow
	for page := 0; page < 4 && url != ""; page++ { // cap pages so we never hang on huge repos
		resp, err := client.Get(url)
		if err != nil {
			return nil, fmt.Errorf("docker hub unreachable: %w", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("docker hub returned %s for %s", resp.Status, repo)
		}
		var body struct {
			Next    string `json:"next"`
			Results []struct {
				Name   string `json:"name"`
				Images []struct {
					Architecture string `json:"architecture"`
				} `json:"images"`
			} `json:"results"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("docker hub response parse failed: %w", err)
		}
		for _, t := range body.Results {
			if t.Name == "" || t.Name[0] < '0' || t.Name[0] > '9' {
				continue // skip non-version tags (latest, dev-latest, ...)
			}
			added := false
			for _, img := range t.Images {
				arch := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[img.Architecture]
				if arch == "" {
					continue
				}
				out = append(out, versionRow{version: t.Name, arch: arch})
				added = true
			}
			if !added {
				out = append(out, versionRow{version: t.Name})
			}
		}
		url = body.Next
	}
	return out, nil
}

func cleanVersion(v string, strip bool) string {
	if !strip {
		return v
	}
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i]
	}
	return v
}

// majorOf returns the leading numeric run of a version ("18.4"->"18",
// "9.6.24"->"9", "18~rc1"->"18"). Falls back to the whole string when it does
// not start with a digit (e.g. operator tag "main").
func majorOf(v string) string {
	i := 0
	for i < len(v) && v[i] >= '0' && v[i] <= '9' {
		i++
	}
	if i == 0 {
		return v
	}
	return v[:i]
}

// distinctVersions returns the deduped (and optionally build-stripped) versions
// for a source, filtered by os/arch, sorted newest-first.
func distinctVersions(rows []versionRow, s versionSource, osFilter, archFilter string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if osFilter != "" && r.os != "" && r.os != osFilter {
			continue
		}
		if archFilter != "" && r.arch != "" && r.arch != archFilter {
			continue
		}
		v := cleanVersion(r.version, s.StripBuild)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return versionLess(out[j], out[i]) }) // newest first
	return out
}

// latestPerMajor keeps only the newest version within each major series,
// preserving the newest-first ordering of the input.
func latestPerMajor(versions []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range versions { // input already newest-first
		m := majorOf(v)
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, v)
	}
	return out
}

func runVersions(logger *log.Logger, dbFile string, args []string,
	osFilter, archFilter string, latest, all, asJSON bool) {

	sources, err := loadVersionSources(dbFile)
	if err != nil {
		logger.Println("Error reading version sources:", err)
		return
	}
	if len(sources) == 0 {
		logger.Println("No version sources found. Try: anydbver update")
		return
	}

	// Detail view for a single software.
	if len(args) >= 1 {
		want := strings.ToLower(args[0])
		var match *versionSource
		for i := range sources {
			if strings.ToLower(sources[i].Keyword) == want {
				match = &sources[i]
				break
			}
		}
		if match == nil { // be forgiving: substring match on keyword or display name
			for i := range sources {
				if strings.Contains(strings.ToLower(sources[i].Keyword), want) ||
					strings.Contains(strings.ToLower(sources[i].DisplayName), want) {
					match = &sources[i]
					break
				}
			}
		}
		if match == nil {
			logger.Printf("Unknown software %q. Run 'anydbver versions' to see available keywords.\n", args[0])
			return
		}
		printVersionDetail(logger, dbFile, *match, osFilter, archFilter, latest, all, asJSON)
		return
	}

	// Overview of everything.
	printVersionOverview(logger, dbFile, sources, osFilter, archFilter, asJSON)
}

func printVersionOverview(logger *log.Logger, dbFile string, sources []versionSource,
	osFilter, archFilter string, asJSON bool) {

	type overviewItem struct {
		Keyword     string `json:"keyword"`
		DisplayName string `json:"display_name"`
		Count       int    `json:"count"`
		Latest      string `json:"latest"`
	}
	var items []overviewItem
	for _, s := range sources {
		rows, err := queryVersionRows(dbFile, s)
		if err != nil {
			// Don't drop the row (e.g. Docker Hub offline) — show it as unavailable.
			items = append(items, overviewItem{s.Keyword, s.DisplayName, 0, "(unavailable)"})
			continue
		}
		vers := distinctVersions(rows, s, osFilter, archFilter)
		latest := ""
		if len(vers) > 0 {
			latest = vers[0]
		}
		items = append(items, overviewItem{s.Keyword, s.DisplayName, len(vers), latest})
	}

	if asJSON {
		b, _ := json.MarshalIndent(items, "", "  ")
		fmt.Println(string(b))
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "SOFTWARE\tKEYWORD\tVERSIONS\tLATEST")
	for _, it := range items {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\n", it.DisplayName, it.Keyword, it.Count, it.Latest)
	}
	w.Flush()
	fmt.Println("\nRun 'anydbver versions <keyword>' for the full list (e.g. anydbver versions pg).")
}

func printVersionDetail(logger *log.Logger, dbFile string, s versionSource,
	osFilter, archFilter string, latest, all, asJSON bool) {

	rows, err := queryVersionRows(dbFile, s)
	if err != nil {
		logger.Println("Error:", err)
		return
	}

	// --all: raw version strings with their os/arch availability.
	if all {
		printVersionAll(s, rows, osFilter, archFilter, asJSON)
		return
	}

	vers := distinctVersions(rows, s, osFilter, archFilter)
	if latest {
		vers = latestPerMajor(vers)
	}

	if asJSON {
		out := struct {
			Keyword     string   `json:"keyword"`
			DisplayName string   `json:"display_name"`
			Versions    []string `json:"versions"`
		}{s.Keyword, s.DisplayName, vers}
		b, _ := json.MarshalIndent(out, "", "  ")
		fmt.Println(string(b))
		return
	}

	filterNote := ""
	if osFilter != "" {
		filterNote += " os=" + osFilter
	}
	if archFilter != "" {
		filterNote += " arch=" + archFilter
	}
	fmt.Printf("%s (%s) — %d versions%s\n", s.DisplayName, s.Keyword, len(vers), filterNote)
	if len(vers) == 0 {
		return
	}

	if s.StripBuild {
		// Group by major series, one line each.
		var majors []string
		byMajor := map[string][]string{}
		for _, v := range vers {
			m := majorOf(v)
			if _, ok := byMajor[m]; !ok {
				majors = append(majors, m)
			}
			byMajor[m] = append(byMajor[m], v)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		for _, m := range majors {
			fmt.Fprintf(w, "  %s\t%s\n", m, strings.Join(byMajor[m], "  "))
		}
		w.Flush()
		if s.SourceTable == "docker_hub" {
			fmt.Printf("\ntip: deploy with  %s:%s  (image tag pulled from Docker Hub: %s)\n",
				s.Keyword, majorOf(vers[0]), s.Program)
		} else {
			fmt.Printf("\ntip: deploy with  %s:%s  (anydbver picks the newest build for your OS/arch)\n",
				s.Keyword, majorOf(vers[0]))
		}
	} else {
		for _, v := range vers {
			fmt.Printf("  %s\n", v)
		}
		fmt.Printf("\ntip: deploy with  %s:%s\n", s.Keyword, vers[0])
	}
}

func printVersionAll(s versionSource, rows []versionRow, osFilter, archFilter string, asJSON bool) {
	// version -> set of "os/arch"
	platforms := map[string]map[string]bool{}
	var order []string
	for _, r := range rows {
		if osFilter != "" && r.os != "" && r.os != osFilter {
			continue
		}
		if archFilter != "" && r.arch != "" && r.arch != archFilter {
			continue
		}
		if _, ok := platforms[r.version]; !ok {
			platforms[r.version] = map[string]bool{}
			order = append(order, r.version)
		}
		p := strings.TrimSuffix(r.os+"/"+r.arch, "/")
		if p != "" {
			platforms[r.version][p] = true
		}
	}
	sort.Slice(order, func(i, j int) bool { return versionLess(order[j], order[i]) })

	if asJSON {
		type item struct {
			Version   string   `json:"version"`
			Platforms []string `json:"platforms"`
		}
		var items []item
		for _, v := range order {
			items = append(items, item{v, sortedKeys(platforms[v])})
		}
		b, _ := json.MarshalIndent(items, "", "  ")
		fmt.Println(string(b))
		return
	}

	fmt.Printf("%s (%s) — %d package versions\n", s.DisplayName, s.Keyword, len(order))
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	for _, v := range order {
		fmt.Fprintf(w, "  %s\t%s\n", v, strings.Join(sortedKeys(platforms[v]), ", "))
	}
	w.Flush()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// packageArch is the arch the version database uses for the machine anydbver
// runs on. Containers share the host kernel, so an Apple Silicon Mac can only
// install aarch64 packages.
func packageArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}

// versionAvailability reports whether a requested version prefix matches
// anything in the source's table, and whether it matches for this machine's
// architecture. Sources whose versions are fetched from the network
// (docker_hub) are treated as existing: a deploy should not depend on a remote
// lookup succeeding.
func versionAvailability(dbFile string, s versionSource, version string) (exists bool, forArch bool, err error) {
	if s.SourceTable == "docker_hub" || !allowedVersionTables[s.SourceTable] {
		return true, true, nil
	}
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		return true, true, err
	}
	defer db.Close()

	var where string
	var qargs []interface{}
	switch s.SourceTable {
	case "k8s_operators_version":
		where = `name = ? AND version LIKE ?`
		qargs = append(qargs, s.Program, version+"%")
	case "general_version":
		where = `program = ? AND version LIKE ?`
		qargs = append(qargs, s.Program, version+"%")
	default:
		where = `version LIKE ?`
		qargs = append(qargs, version+"%")
	}

	var found int
	err = db.QueryRow(`SELECT 1 FROM `+s.SourceTable+` WHERE `+where+` LIMIT 1`, qargs...).Scan(&found)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return true, true, err
	}

	// A table without an arch column is arch-agnostic, and so is a row that
	// leaves the column empty.
	if !tableHasColumn(db, s.SourceTable, "arch") {
		return true, true, nil
	}
	archArgs := append(append([]interface{}{}, qargs...), packageArch())
	err = db.QueryRow(`SELECT 1 FROM `+s.SourceTable+` WHERE `+where+
		` AND (arch = ? OR arch = '' OR arch IS NULL) LIMIT 1`, archArgs...).Scan(&found)
	if err == sql.ErrNoRows {
		return true, false, nil
	}
	if err != nil {
		return true, true, err
	}
	return true, true, nil
}

// tableHasColumn reports whether table has the named column. The table name is
// whitelisted by allowedVersionTables before it gets here.
func tableHasColumn(db *sql.DB, table string, column string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

// knownVersionsFor returns the newest few versions of a software, for telling
// the user what they could have asked for instead.
func knownVersionsFor(dbFile string, s versionSource, limit int) []string {
	return knownVersionsForArch(dbFile, s, "", limit)
}

// knownVersionsForArch is knownVersionsFor limited to the versions this machine
// can actually install.
func knownVersionsForArch(dbFile string, s versionSource, arch string, limit int) []string {
	rows, err := queryVersionRows(dbFile, s)
	if err != nil {
		return nil
	}
	all := distinctVersions(rows, s, "", arch)
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}
