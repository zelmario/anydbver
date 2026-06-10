package main

// Chaos engine (step 1): host-portable network fault injection for anydbver's
// docker containers, driven by tc/netem applied *inside each container's network
// namespace*. We do NOT install anything in the target containers and we do NOT
// touch the host: a short-lived helper container joins the target's netns
// (`docker run --net=container:<target> --cap-add=NET_ADMIN`) and runs tc there.
// The qdisc persists on the target's eth0 after the helper exits.
//
// Why netns-side and not host-side veth: under Docker Desktop (incl. WSL2) the
// docker engine runs in its own VM, so container veths are NOT visible from the
// host where the CLI runs. Shaping inside the netns works identically on native
// Linux and Docker Desktop.
//
// Mechanism (the "armed but harmless" pattern):
//   - root qdisc on eth0: a prio qdisc with one band PER PEER plus a final
//     untouched default band. The priomap sends ALL default traffic to that last
//     band, so every band stays empty until a filter targets it.
//   - one netem qdisc per peer band, each carrying that peer's own delay/jitter/
//     loss profile — so a node that is an endpoint of several links with DIFFERENT
//     profiles (50ms to one peer, 100% loss to another) shapes each independently.
//   - u32 filters matching a specific peer IP redirect only that peer's traffic
//     into that peer's band.
//
// A link A<->B is made symmetric by shaping eth0 egress on BOTH containers and
// filtering the peer's IP on each (egress dst-match is the meaningful direction;
// src is added belt-and-suspenders).
//
// The CLI is stateless across invocations, so the desired link set is persisted
// to a per-namespace JSON file (chaosLoadState/chaosSaveState) and every change
// rebuilds all shaping from it (chaosReconcileLinks) — the same full-rebuild the
// dashboard does from its in-memory set. This is what lets a second CLI command
// (`chaos partition`) add a link without clobbering the first (`chaos link`).

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

const (
	// chaosMaxPeers is the most peers we can shape on one node: tc prio supports
	// up to 16 bands and we reserve one for the untouched default.
	chaosMaxPeers      = 15
	chaosDefaultTTLSec = 3600 // dead-man switch: auto-clear after 1h
	// chaosTcImageDefault is a tiny image whose entrypoint is tc. Overridable via
	// ANYDBVER_TC_IMAGE. (Folding iproute-tc into the anydbver ansible image at a
	// future release would drop this external dependency.)
	chaosTcImageDefault = "gaiadocker/iproute2"
	// chaosPingImageDefault carries `ping` (the el9 nodes don't). Overridable via
	// ANYDBVER_PING_IMAGE. busybox is tiny and has a working ping.
	chaosPingImageDefault = "busybox"
)

type chaosNode struct {
	Node      string // short name as the user knows it, e.g. node0
	Container string // full docker name, e.g. ns-user-node0
	IP        string
	State     string // docker state: running | paused | exited | created
}

// chaosNetemParams holds a netem profile parsed from key=value CLI args. Any
// numeric field may be a single value ("5%", "100ms") or a "min-max" range
// ("3-6", "50-200ms") that is rolled to a random concrete value each time it is
// applied (so a periodic re-apply makes conditions drift — see flux mode).
type chaosNetemParams struct {
	Delay     string // e.g. "100ms" or "50-200ms"
	Jitter    string // e.g. "20ms"
	Loss      string // e.g. "5%" or "3-6"
	LossCorr  string // loss correlation % → bursty/clustered loss
	Corrupt   string // % of packets with a random bit error
	Duplicate string // % of packets duplicated
	Reorder   string // % of packets reordered (needs a delay; one is added if absent)
	Rate      string // bandwidth cap, e.g. "1mbit"
}

func (p chaosNetemParams) empty() bool {
	return p.Delay == "" && p.Loss == "" && p.Corrupt == "" &&
		p.Duplicate == "" && p.Reorder == "" && p.Rate == ""
}

// chaosRangeRe matches "lo-hi" with optional units on either side, e.g.
// "3-6", "50-200ms", "1-5mbit".
var chaosRangeRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)([a-zA-Z%]*)-(\d+(?:\.\d+)?)([a-zA-Z%]*)$`)

// chaosRollValue returns s unchanged unless it is a "min-max" range, in which
// case it picks a random integer value in [min,max] and re-attaches the unit.
func chaosRollValue(s string) string {
	s = strings.TrimSpace(s)
	m := chaosRangeRe.FindStringSubmatch(s)
	if m == nil {
		return s
	}
	lo, err1 := strconv.ParseFloat(m[1], 64)
	hi, err2 := strconv.ParseFloat(m[3], 64)
	if err1 != nil || err2 != nil {
		return s
	}
	unit := m[4]
	if unit == "" {
		unit = m[2]
	}
	if hi < lo {
		lo, hi = hi, lo
	}
	v := lo
	if hi > lo {
		v = lo + rand.Float64()*(hi-lo)
	}
	return strconv.Itoa(int(v+0.5)) + unit
}

// chaosPct rolls a possible range then ensures a trailing "%".
func chaosPct(s string) string {
	s = chaosRollValue(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	if !strings.HasSuffix(s, "%") {
		s += "%"
	}
	return s
}

// netemArgs renders the params into a `tc qdisc ... netem ...` argument tail,
// rolling any ranges to concrete random values.
func (p chaosNetemParams) netemArgs() []string {
	delay := chaosRollValue(p.Delay)
	jitter := chaosRollValue(p.Jitter)
	reorder := chaosPct(p.Reorder)
	// reorder is only meaningful with a delay; give it a small one if absent.
	if reorder != "" && delay == "" {
		delay = "10ms"
	}

	args := []string{"netem"}
	if delay != "" {
		args = append(args, "delay", delay)
		if jitter != "" {
			args = append(args, jitter)
		}
	}
	if loss := chaosPct(p.Loss); loss != "" {
		args = append(args, "loss", loss)
		if corr := chaosPct(p.LossCorr); corr != "" {
			args = append(args, corr) // correlation → bursty loss
		}
	}
	if c := chaosPct(p.Corrupt); c != "" {
		args = append(args, "corrupt", c)
	}
	if d := chaosPct(p.Duplicate); d != "" {
		args = append(args, "duplicate", d)
	}
	if reorder != "" {
		args = append(args, "reorder", reorder)
	}
	if p.Rate != "" {
		args = append(args, "rate", chaosRollValue(p.Rate))
	}
	return args
}

func (p chaosNetemParams) human() string {
	parts := []string{}
	if p.Delay != "" {
		d := "delay " + p.Delay
		if p.Jitter != "" {
			d += " ±" + p.Jitter
		}
		parts = append(parts, d)
	}
	if p.Loss != "" {
		l := "loss " + p.Loss
		if p.LossCorr != "" {
			l += " (bursty " + p.LossCorr + ")"
		}
		parts = append(parts, l)
	}
	if p.Corrupt != "" {
		parts = append(parts, "corrupt "+p.Corrupt)
	}
	if p.Duplicate != "" {
		parts = append(parts, "dup "+p.Duplicate)
	}
	if p.Reorder != "" {
		parts = append(parts, "reorder "+p.Reorder)
	}
	if p.Rate != "" {
		parts = append(parts, "rate "+p.Rate)
	}
	if len(parts) == 0 {
		return "(no effect)"
	}
	return strings.Join(parts, ", ")
}

// --- input normalization + validation ---------------------------------------

const (
	chaosKindTime = iota // delay/jitter: number + optional ms/us/s
	chaosKindPct         // loss/corrupt/...: number 0-100, optional %
	chaosKindRate        // rate: number + optional [kmgt]bit
)

var (
	chaosReScalarPct  = regexp.MustCompile(`^\d+(\.\d+)?%?$`)
	chaosReScalarTime = regexp.MustCompile(`(?i)^\d+(\.\d+)?(ms|us|µs|s)?$`)
	chaosReScalarRate = regexp.MustCompile(`(?i)^\d+(\.\d+)?((k|m|g|t)?bit)?$`)
)

// chaosCleanSpaces strips all whitespace so "45 %" == "45%", "1 mbit" == "1mbit".
func chaosCleanSpaces(s string) string { return strings.Join(strings.Fields(s), "") }

// chaosNormTime cleans whitespace and appends "ms" when no time unit is present,
// so bare "100" or range "50-200" become valid netem times.
func chaosNormTime(s string) string {
	s = chaosCleanSpaces(s)
	if s == "" {
		return ""
	}
	// Only touch numeric-looking input; leave garbage for validate() to report verbatim.
	if c := s[0]; c != '.' && (c < '0' || c > '9') {
		return s
	}
	low := strings.ToLower(s)
	if strings.HasSuffix(low, "ms") || strings.HasSuffix(low, "us") || strings.HasSuffix(low, "µs") || strings.HasSuffix(low, "s") {
		return s
	}
	return s + "ms"
}

// normalize tidies every field in place (whitespace, implicit units).
func (p *chaosNetemParams) normalize() {
	p.Delay = chaosNormTime(p.Delay)
	p.Jitter = chaosNormTime(p.Jitter)
	p.Loss = chaosCleanSpaces(p.Loss)
	p.LossCorr = chaosCleanSpaces(p.LossCorr)
	p.Corrupt = chaosCleanSpaces(p.Corrupt)
	p.Duplicate = chaosCleanSpaces(p.Duplicate)
	p.Reorder = chaosCleanSpaces(p.Reorder)
	p.Rate = chaosCleanSpaces(p.Rate)
}

// validate returns a friendly error for the first malformed field, or nil.
func (p chaosNetemParams) validate() error {
	checks := []struct {
		name string
		val  string
		kind int
	}{
		{"delay", p.Delay, chaosKindTime}, {"jitter", p.Jitter, chaosKindTime},
		{"loss", p.Loss, chaosKindPct}, {"loss correlation", p.LossCorr, chaosKindPct},
		{"corrupt", p.Corrupt, chaosKindPct}, {"duplicate", p.Duplicate, chaosKindPct},
		{"reorder", p.Reorder, chaosKindPct}, {"rate", p.Rate, chaosKindRate},
	}
	for _, c := range checks {
		if c.val == "" {
			continue
		}
		if err := chaosValidateValue(c.val, c.kind); err != nil {
			return fmt.Errorf("%s %q: %v", c.name, c.val, err)
		}
	}
	return nil
}

// chaosValidateValue validates a single value or a "lo-hi" range.
func chaosValidateValue(val string, kind int) error {
	parts := strings.Split(val, "-")
	if len(parts) > 2 {
		return fmt.Errorf("malformed range")
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("empty value (no negatives)")
		}
		if err := chaosValidateScalar(part, kind); err != nil {
			return err
		}
	}
	return nil
}

func chaosValidateScalar(s string, kind int) error {
	switch kind {
	case chaosKindPct:
		if !chaosReScalarPct.MatchString(s) {
			return fmt.Errorf("must be a percentage like 45 or 45%%")
		}
		v, _ := strconv.ParseFloat(strings.TrimSuffix(s, "%"), 64)
		if v < 0 || v > 100 {
			return fmt.Errorf("must be between 0 and 100")
		}
	case chaosKindTime:
		if !chaosReScalarTime.MatchString(s) {
			return fmt.Errorf("must be a time like 100ms, 0.1s or 50-200ms")
		}
	case chaosKindRate:
		if !chaosReScalarRate.MatchString(s) {
			return fmt.Errorf("must be a rate like 1mbit, 100kbit")
		}
	}
	return nil
}

func chaosTcImage() string {
	if v := os.Getenv("ANYDBVER_TC_IMAGE"); v != "" {
		return v
	}
	return chaosTcImageDefault
}

func chaosPingImage() string {
	if v := os.Getenv("ANYDBVER_PING_IMAGE"); v != "" {
		return v
	}
	return chaosPingImageDefault
}

var (
	chaosPingCountsRe = regexp.MustCompile(`(\d+) packets transmitted, (\d+) (?:packets )?received`)
	chaosPingTimeRe   = regexp.MustCompile(`time[=<]([\d.]+)`)
	chaosPingMissRe   = regexp.MustCompile(`(?i)not found|no such file|executable file not found|command not found`)
)

// chaosMeasureRTT pings targetIP from inside container's network namespace and
// returns the average round-trip time (ms) and packet-loss %. A 100% loss result
// means the path is severed (partition) — rtt is then meaningless.
//
// It prefers the node's own `ping` (iputils → clean, reliable output) and only
// falls back to a busybox helper in the node's netns when ping isn't installed
// (e.g. minimal el9 images). Loss is derived from the transmitted/received counts
// rather than ping's "% loss" field, and RTT is averaged over only sane per-packet
// times — busybox in a shared netns can emit a stale reply with an absurd time and
// a received-count larger than transmitted, which corrupts both of those fields.
func chaosMeasureRTT(logger *log.Logger, container, targetIP string) (float64, int) {
	// 1) in-container ping (iputils)
	args := []string{"docker", "exec", container, "ping", "-c", "5", "-W", "1", "-w", "8", targetIP}
	out, _ := runtools.RunGetOutput(logger, args, "ping", regexp.MustCompile(".*"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
	if chaosPingMissRe.MatchString(out) {
		// 2) fall back to a busybox helper sharing the node's network namespace
		args = []string{"docker", "run", "--rm",
			"--net=container:" + container, "--cap-add=NET_RAW",
			chaosPingImage(), "ping", "-c", "5", "-w", "8", targetIP}
		out, _ = runtools.RunGetOutput(logger, args, "ping", regexp.MustCompile(".*"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
	}
	return chaosParsePing(out)
}

func chaosParsePing(out string) (float64, int) {
	transmitted, received := 0, 0
	times := []float64{}
	for _, line := range strings.Split(out, "\n") {
		if m := chaosPingCountsRe.FindStringSubmatch(line); m != nil {
			transmitted, _ = strconv.Atoi(m[1])
			received, _ = strconv.Atoi(m[2])
		}
		if m := chaosPingTimeRe.FindStringSubmatch(line); m != nil {
			// Drop stale-reply outliers (a leftover echo from a prior run in the
			// same netns can show up with a multi-second time).
			if v, err := strconv.ParseFloat(m[1], 64); err == nil && v >= 0 && v < 100000 {
				times = append(times, v)
			}
		}
	}

	// The first reply pays one-off neighbor/ARP resolution (itself delayed by any
	// netem on the path), which skews a small average. Drop it when we have enough
	// samples so the reported RTT reflects the steady-state induced latency.
	avgTimes := times
	if len(times) >= 3 {
		avgTimes = times[1:]
	}
	rtt := 0.0
	if len(avgTimes) > 0 {
		sum := 0.0
		for _, t := range avgTimes {
			sum += t
		}
		rtt = sum / float64(len(avgTimes))
	}

	// Loss from counts, clamped — robust against busybox's buggy "% loss" field.
	if transmitted <= 0 || len(times) == 0 {
		return rtt, 100 // never got a usable reply
	}
	if received > transmitted {
		received = transmitted // ignore duplicate/stale replies
	}
	loss := (transmitted - received) * 100 / transmitted
	if loss < 0 {
		loss = 0
	}
	return rtt, loss
}

// --- persistent helper containers -------------------------------------------
// Each `docker run --rm` helper costs hundreds of ms to multiple seconds to start
// (≈1.9s on Docker Desktop / WSL2), which makes the dashboard feel sluggish since
// every apply spawns one per node. Instead we keep ONE long-lived helper container
// per node (sharing that node's netns) and drive tc through `docker exec`, which is
// ~50ms. The helper is created lazily on the first shaping op and reused; it is
// removed on clear / dashboard shutdown / dead-man / destroy. Helpers join via
// --net=container:<node> so they are NOT on the namespace bridge network and never
// show up as nodes in chaosListNodes / anydbver list.

func chaosHelperName(container string) string { return "chaos-helper-" + container }

// chaosHelperRunning reports whether the node's helper container exists and is up.
func chaosHelperRunning(logger *log.Logger, container string) bool {
	out, _ := runtools.RunGetOutput(logger,
		[]string{"docker", "inspect", "-f", "{{.State.Running}}", chaosHelperName(container)},
		"inspect", regexp.MustCompile(".*"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
	return strings.TrimSpace(out) == "true"
}

// chaosEnsureHelper makes sure the node's helper container is running and returns
// its name. A stale (non-running) helper is removed and recreated.
func chaosEnsureHelper(logger *log.Logger, container string) (string, error) {
	helper := chaosHelperName(container)
	if chaosHelperRunning(logger, container) {
		return helper, nil
	}
	// Remove any stale/exited helper, then start a fresh one that just sleeps.
	runtools.RunGetOutput(logger, []string{"docker", "rm", "-f", helper},
		"rm helper", regexp.MustCompile(".*"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
	args := []string{"docker", "run", "-d", "--name", helper,
		"--net=container:" + container, "--cap-add=NET_ADMIN",
		"--entrypoint", "sh", chaosTcImage(), "-c", "while :; do sleep 3600; done"}
	if _, err := runtools.RunGetOutput(logger, args, "start helper",
		regexp.MustCompile("ignore this"), false, map[string]string{}, runtools.COMMAND_TIMEOUT); err != nil {
		return helper, fmt.Errorf("starting helper for %s: %w", container, err)
	}
	return helper, nil
}

// chaosRemoveHelpers force-removes the helper containers for the given nodes.
func chaosRemoveHelpers(logger *log.Logger, containers []string) {
	if len(containers) == 0 {
		return
	}
	args := []string{"docker", "rm", "-f"}
	for _, c := range containers {
		args = append(args, chaosHelperName(c))
	}
	runtools.RunGetOutput(logger, args, "rm helpers",
		regexp.MustCompile(".*"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
}

// chaosCleanupHelpers removes every node's helper container in the namespace.
func chaosCleanupHelpers(logger *log.Logger, provider, namespace string) {
	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return
	}
	containers := make([]string, 0, len(nodes))
	for _, n := range nodes {
		containers = append(containers, n.Container)
	}
	chaosRemoveHelpers(logger, containers)
}

// chaosTc runs a tc command in the target's netns. It uses the node's persistent
// helper via `docker exec` when one is running (fast), and otherwise falls back to
// a throwaway `docker run` helper — so a clear/status still works for shaping that
// was applied without a persistent helper. It never creates a helper itself.
// `ignore` is a regexp of output to treat as success (use ".*" for best-effort).
func chaosTc(logger *log.Logger, container string, ignore string, tcArgs ...string) (string, error) {
	var args []string
	if chaosHelperRunning(logger, container) {
		args = append([]string{"docker", "exec", chaosHelperName(container), "tc"}, tcArgs...)
	} else {
		args = append([]string{"docker", "run", "--rm",
			"--net=container:" + container, "--cap-add=NET_ADMIN",
			"--entrypoint", "tc", chaosTcImage()}, tcArgs...)
	}

	ignoreRe := regexp.MustCompile("ignore this")
	if ignore != "" {
		ignoreRe = regexp.MustCompile(ignore)
	}
	return runtools.RunGetOutput(logger, args, "tc command failed", ignoreRe, false, map[string]string{}, runtools.COMMAND_TIMEOUT)
}

// chaosListNodes returns the docker containers attached to the namespace network.
func chaosListNodes(logger *log.Logger, provider string, namespace string) ([]chaosNode, error) {
	if provider != "docker" {
		return nil, fmt.Errorf("chaos currently supports only the docker provider (got %q)", provider)
	}
	network := getNetworkName(logger, namespace)
	// -a so paused/exited nodes still appear (their network membership persists).
	args := []string{"docker", "ps", "-a", "--filter", "network=" + network, "--format", "{{.Names}}\t{{.State}}"}
	out, err := runtools.RunGetOutput(logger, args, "Error listing containers", regexp.MustCompile("ignore this"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
	if err != nil {
		return nil, err
	}

	prefix := strings.TrimSuffix(network, "anydbver") // ns-user-  (or user-)

	nodes := []chaosNode{}
	names := []string{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		name := strings.TrimSpace(fields[0])
		state := ""
		if len(fields) > 1 {
			state = strings.TrimSpace(fields[1])
		}
		if name == "" {
			continue
		}
		short := strings.TrimPrefix(name, prefix)
		nodes = append(nodes, chaosNode{Node: short, Container: name, State: state})
		names = append(names, name)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no containers found on network %q (deploy something first)", network)
	}

	// One docker inspect for all IPs instead of one per node.
	ips := chaosInspectIPs(logger, network, names)
	for i := range nodes {
		nodes[i].IP = ips[nodes[i].Container]
	}
	return nodes, nil
}

// chaosInspectIPs returns container-name -> IP on the given network in a single
// docker inspect call.
func chaosInspectIPs(logger *log.Logger, network string, names []string) map[string]string {
	m := map[string]string{}
	if len(names) == 0 {
		return m
	}
	args := append([]string{"docker", "inspect"}, names...)
	args = append(args, "--format", `{{.Name}}|{{index .NetworkSettings.Networks "`+network+`" "IPAddress"}}`)
	out, err := runtools.RunGetOutput(logger, args, "Error inspecting containers", regexp.MustCompile("ignore this"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "|", 2)
		if len(parts) != 2 {
			continue
		}
		m[strings.TrimPrefix(parts[0], "/")] = strings.TrimSpace(parts[1])
	}
	return m
}

// chaosResolveNode finds a node by the short name the user typed (node0) or by
// full container name.
func chaosResolveNode(logger *log.Logger, namespace string, nodes []chaosNode, want string) (*chaosNode, error) {
	full := anydbver_common.MakeContainerHostName(logger, namespace, want)
	for i := range nodes {
		n := &nodes[i]
		if n.Node == want || n.Container == want || n.Container == full {
			return n, nil
		}
	}
	names := []string{}
	for _, n := range nodes {
		names = append(names, n.Node)
	}
	return nil, fmt.Errorf("node %q not found; available: %s", want, strings.Join(names, ", "))
}

// chaosSh runs a shell script inside the target container's network namespace via
// that node's persistent helper container (created on demand), as one `docker exec`.
// Batching every tc command for a node into a single script keeps it to one exec.
func chaosSh(logger *log.Logger, container, script string) (string, error) {
	helper, err := chaosEnsureHelper(logger, container)
	if err != nil {
		return "", err
	}
	args := []string{"docker", "exec", helper, "sh", "-c", script}
	return runtools.RunGetOutput(logger, args, "tc batch failed", regexp.MustCompile("ignore this"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
}

// chaosPeerShape is one peer IP and the netem profile to apply to traffic for it.
type chaosPeerShape struct {
	IP     string
	Params chaosNetemParams
}

// chaosShapeContainer rebuilds eth0's shaping from scratch so each peer gets its
// OWN prio band + netem qdisc, in a single helper-container run. The last band is
// an untouched default that every unmatched flow passes through. Giving each peer
// its own band is what fixes the single-band bug where a node in two links with
// different profiles applied only the last one to BOTH peers.
func chaosShapeContainer(logger *log.Logger, container string, peers []chaosPeerShape) error {
	// Dedupe by IP (a pair should appear once, but be safe); last entry wins.
	seen := map[string]int{}
	uniq := []chaosPeerShape{}
	for _, p := range peers {
		if p.IP == "" {
			continue
		}
		if idx, ok := seen[p.IP]; ok {
			uniq[idx] = p
			continue
		}
		seen[p.IP] = len(uniq)
		uniq = append(uniq, p)
	}
	if len(uniq) == 0 {
		chaosClearContainer(logger, container)
		return nil
	}
	if len(uniq) > chaosMaxPeers {
		logger.Printf("chaos: %s has %d peers to shape; tc allows %d (prio band limit) — shaping the first %d",
			container, len(uniq), chaosMaxPeers, chaosMaxPeers)
		uniq = uniq[:chaosMaxPeers]
	}

	n := len(uniq)
	bands := n + 1 // one band per peer + a final untouched default band
	// priomap: send every TOS to the last (default) band, index n.
	pri := make([]string, 16)
	for i := range pri {
		pri[i] = strconv.Itoa(n)
	}

	var b strings.Builder
	b.WriteString("tc qdisc del dev eth0 root 2>/dev/null; ")
	fmt.Fprintf(&b, "tc qdisc add dev eth0 root handle 1: prio bands %d priomap %s; ", bands, strings.Join(pri, " "))
	for i, p := range uniq {
		band := i + 1 // class 1:1 .. 1:n  (1:bands is the untouched default)
		netem := append([]string{"tc", "qdisc", "replace", "dev", "eth0",
			"parent", fmt.Sprintf("1:%d", band), "handle", fmt.Sprintf("%d:", 10+band)}, p.Params.netemArgs()...)
		fmt.Fprintf(&b, "%s; ", strings.Join(netem, " "))
		for _, dir := range []string{"dst", "src"} {
			fmt.Fprintf(&b, "tc filter add dev eth0 protocol ip parent 1:0 prio 1 u32 match ip %s %s/32 flowid 1:%d; ", dir, p.IP, band)
		}
	}
	if _, err := chaosSh(logger, container, b.String()); err != nil {
		return fmt.Errorf("shaping %s: %w", container, err)
	}
	return nil
}

// chaosReconcileLinks rebuilds ALL shaping in the namespace from the given desired
// link set: every running/paused node is reshaped with one band per peer, or
// cleared if it is in no link. Shared by the CLI (driven by the per-namespace
// state file) and the dashboard (driven by its in-memory set).
func chaosReconcileLinks(logger *log.Logger, provider, namespace string, links []chaosLinkState) error {
	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return err
	}
	byName := map[string]chaosNode{}
	for _, n := range nodes {
		byName[n.Node] = n
	}
	perContainer := map[string][]chaosPeerShape{}
	add := func(self, peer chaosNode, p chaosNetemParams) {
		if self.IP == "" || peer.IP == "" {
			return
		}
		perContainer[self.Container] = append(perContainer[self.Container], chaosPeerShape{IP: peer.IP, Params: p})
	}
	for _, l := range links {
		a, b := byName[l.A], byName[l.B]
		add(a, b, l.Params)
		add(b, a, l.Params)
	}

	// A node with no helper has no shaping (the invariant: all shaping is applied
	// through the node's helper), so unshaped-and-undesired nodes need no work —
	// only clear nodes that currently have a helper. One docker ps avoids a
	// per-node inspect.
	hasHelper := chaosExistingHelpers(logger)

	var wg sync.WaitGroup
	for _, n := range nodes {
		if n.State != "running" && n.State != "paused" {
			continue
		}
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			if peers := perContainer[c]; len(peers) > 0 {
				if err := chaosShapeContainer(logger, c, peers); err != nil {
					logger.Printf("chaos: shape %s: %v", c, err)
				}
			} else if hasHelper[chaosHelperName(c)] {
				chaosClearContainer(logger, c) // had shaping, now in no link
			}
		}(n.Container)
	}
	wg.Wait()
	return nil
}

// chaosExistingHelpers returns the set of chaos helper container names that exist
// right now (running or not), in one docker ps call.
func chaosExistingHelpers(logger *log.Logger) map[string]bool {
	out, _ := runtools.RunGetOutput(logger,
		[]string{"docker", "ps", "-a", "--filter", "name=chaos-helper-", "--format", "{{.Names}}"},
		"ps helpers", regexp.MustCompile(".*"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
	set := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			set[line] = true
		}
	}
	return set
}

// --- per-namespace CLI state -------------------------------------------------
// The CLI persists the desired link set so separate invocations accumulate.

type chaosLinkPersist struct {
	A      string           `json:"a"`
	B      string           `json:"b"`
	Params chaosNetemParams `json:"params"`
}

func chaosStatePath(logger *log.Logger, namespace string) string {
	return filepath.Join(anydbver_common.GetCacheDirectory(logger),
		anydbver_common.MakeContainerHostName(logger, namespace, "chaos_links")+".json")
}

func chaosLoadState(logger *log.Logger, namespace string) []chaosLinkState {
	data, err := os.ReadFile(chaosStatePath(logger, namespace))
	if err != nil {
		return nil
	}
	var persisted []chaosLinkPersist
	if json.Unmarshal(data, &persisted) != nil {
		return nil
	}
	links := make([]chaosLinkState, 0, len(persisted))
	for _, p := range persisted {
		links = append(links, chaosLinkState{A: p.A, B: p.B, Params: p.Params})
	}
	return links
}

func chaosSaveState(logger *log.Logger, namespace string, links []chaosLinkState) {
	path := chaosStatePath(logger, namespace)
	if len(links) == 0 {
		os.Remove(path)
		return
	}
	persisted := make([]chaosLinkPersist, 0, len(links))
	for _, l := range links {
		persisted = append(persisted, chaosLinkPersist{A: l.A, B: l.B, Params: l.Params})
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		logger.Printf("chaos: could not save state file: %v", err)
	}
}

// chaosClearContainer removes the whole root qdisc (netem + filters) from eth0.
func chaosClearContainer(logger *log.Logger, container string) {
	chaosTc(logger, container, ".*", "qdisc", "del", "dev", "eth0", "root")
}

// chaosScheduleDeadMan spawns a detached process that clears ALL of the
// namespace's nodes and removes the CLI state file after ttlSec seconds, so a
// forgotten fault can't cripple a cluster forever. It clears everything (not just
// the latest link's endpoints) so the live shaping and the state file stay
// consistent after it fires.
func chaosScheduleDeadMan(logger *log.Logger, provider, namespace string, ttlSec int) {
	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return
	}
	img := chaosTcImage()
	cmds := []string{fmt.Sprintf("sleep %d", ttlSec)}
	for _, n := range nodes {
		// Clear via a throwaway helper (works whether or not the persistent helper
		// is still around), then remove the persistent helper.
		cmds = append(cmds, fmt.Sprintf(
			"docker run --rm --net=container:%s --cap-add=NET_ADMIN --entrypoint tc %s qdisc del dev eth0 root 2>/dev/null",
			n.Container, img))
		cmds = append(cmds, fmt.Sprintf("docker rm -f %s 2>/dev/null", chaosHelperName(n.Container)))
	}
	cmds = append(cmds, fmt.Sprintf("rm -f '%s'", chaosStatePath(logger, namespace)))
	script := strings.Join(cmds, "; ")

	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // detach into its own session
	if err := cmd.Start(); err != nil {
		logger.Printf("chaos: could not schedule dead-man cleanup: %v", err)
		return
	}
	go func() { _ = cmd.Wait() }() // release the child so it survives our exit
	logger.Printf("chaos: dead-man switch armed — faults auto-clear in %ds", ttlSec)
}

// chaosDegradeLink applies a symmetric netem profile to the A<->B link. It upserts
// the link into the persistent per-namespace state and rebuilds ALL shaping from
// it, so a previous link on a shared node is preserved (each peer keeps its own
// band) instead of being clobbered.
func chaosDegradeLink(logger *log.Logger, provider, namespace, nodeA, nodeB string, params chaosNetemParams, ttlSec int) error {
	if params.empty() {
		params.Delay = "100ms"
		params.Jitter = "20ms"
	}

	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return err
	}
	a, err := chaosResolveNode(logger, namespace, nodes, nodeA)
	if err != nil {
		return err
	}
	b, err := chaosResolveNode(logger, namespace, nodes, nodeB)
	if err != nil {
		return err
	}
	if a.IP == "" || b.IP == "" {
		return fmt.Errorf("could not resolve IPs (%s=%q, %s=%q)", a.Node, a.IP, b.Node, b.IP)
	}

	// Upsert this link into the persistent set, then rebuild everything from it.
	links := chaosLoadState(logger, namespace)
	key := chaosLinkKey(a.Node, b.Node)
	found := false
	for i := range links {
		if chaosLinkKey(links[i].A, links[i].B) == key {
			links[i] = chaosLinkState{A: a.Node, B: b.Node, Params: params}
			found = true
			break
		}
	}
	if !found {
		links = append(links, chaosLinkState{A: a.Node, B: b.Node, Params: params})
	}
	if err := chaosReconcileLinks(logger, provider, namespace, links); err != nil {
		return err
	}
	chaosSaveState(logger, namespace, links)

	logger.Printf("chaos: degraded link %s(%s) <-> %s(%s): %s",
		a.Node, a.IP, b.Node, b.IP, params.human())
	if ttlSec > 0 {
		chaosScheduleDeadMan(logger, provider, namespace, ttlSec)
	}
	return nil
}

// chaosPartition fully severs the A<->B link by dropping 100% of its packets in
// both directions (a clean network partition / split-brain trigger).
func chaosPartition(logger *log.Logger, provider, namespace, nodeA, nodeB string, ttlSec int) error {
	return chaosDegradeLink(logger, provider, namespace, nodeA, nodeB, chaosNetemParams{Loss: "100%"}, ttlSec)
}

// chaosNodeAction runs a docker lifecycle action against a node's container.
// action ∈ pause | unpause | kill | start (stop is treated as kill's softer cousin).
func chaosNodeAction(logger *log.Logger, provider, namespace, node, action string) error {
	switch action {
	case "pause", "unpause", "kill", "start", "stop":
	default:
		return fmt.Errorf("unknown node action %q (use pause|unpause|kill|start)", action)
	}
	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return err
	}
	n, err := chaosResolveNode(logger, namespace, nodes, node)
	if err != nil {
		return err
	}
	args := []string{"docker", action, n.Container}
	if _, err := runtools.RunGetOutput(logger, args, "docker "+action+" failed", regexp.MustCompile("ignore this"), true, map[string]string{}, runtools.COMMAND_TIMEOUT); err != nil {
		return fmt.Errorf("docker %s %s: %w", action, n.Container, err)
	}
	// kill/stop/start change the node's network namespace, which leaves any
	// persistent helper attached to the OLD netns. Drop it so the next shaping op
	// recreates a fresh one. (pause/unpause keep the netns, so the helper stays valid.)
	switch action {
	case "kill", "stop", "start":
		chaosRemoveHelpers(logger, []string{n.Container})
	}
	logger.Printf("chaos: %s %s", action, n.Node)
	return nil
}

// chaosMeasure pings nodeB from nodeA and prints the measured latency, contrasted
// with whatever was induced (one-way ≈ RTT/2, comparable to the per-direction delay).
func chaosMeasure(logger *log.Logger, provider, namespace, nodeA, nodeB string) error {
	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return err
	}
	a, err := chaosResolveNode(logger, namespace, nodes, nodeA)
	if err != nil {
		return err
	}
	b, err := chaosResolveNode(logger, namespace, nodes, nodeB)
	if err != nil {
		return err
	}
	if b.IP == "" {
		return fmt.Errorf("%s has no IP (is it running?)", b.Node)
	}
	rtt, loss := chaosMeasureRTT(logger, a.Container, b.IP)
	if loss >= 100 {
		logger.Printf("chaos: %s -> %s: unreachable (100%% packet loss)", a.Node, b.Node)
		return nil
	}
	lossStr := ""
	if loss > 0 {
		lossStr = fmt.Sprintf(", %d%% loss", loss)
	}
	logger.Printf("chaos: %s -> %s: RTT %.1fms (one-way ~%.1fms)%s", a.Node, b.Node, rtt, rtt/2, lossStr)
	return nil
}

// chaosClearAll removes all tc shaping from every node's eth0 and forgets the
// persistent CLI link state.
func chaosClearAll(logger *log.Logger, provider, namespace string) error {
	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	cleared := 0
	for i := range nodes {
		if nodes[i].State != "running" && nodes[i].State != "paused" {
			continue
		}
		cleared++
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			chaosClearContainer(logger, c)
		}(nodes[i].Container)
	}
	wg.Wait()
	chaosRemoveHelpers(logger, func() []string {
		cs := make([]string, len(nodes))
		for i := range nodes {
			cs[i] = nodes[i].Container
		}
		return cs
	}()) // tear down the persistent helpers
	chaosSaveState(logger, namespace, nil) // drop the state file
	logger.Printf("chaos: cleared shaping on %d node(s)", cleared)
	return nil
}

// chaosStatus prints the current qdisc/filter state for every node.
func chaosStatus(logger *log.Logger, provider, namespace string) error {
	nodes, err := chaosListNodes(logger, provider, namespace)
	if err != nil {
		return err
	}
	for i := range nodes {
		c := nodes[i].Container
		if nodes[i].State != "running" && nodes[i].State != "paused" {
			fmt.Printf("%-12s ip=%-15s state=%-8s [unreachable]\n", nodes[i].Node, nodes[i].IP, nodes[i].State)
			continue
		}
		q, _ := chaosTc(logger, c, ".*", "qdisc", "show", "dev", "eth0")
		f, _ := chaosTc(logger, c, ".*", "filter", "show", "dev", "eth0")
		shaping := "no shaping"
		if strings.Contains(q, "netem") {
			shaping = "ACTIVE"
		}
		fmt.Printf("%-12s ip=%-15s state=%-8s [%s]\n", nodes[i].Node, nodes[i].IP, nodes[i].State, shaping)
		if strings.TrimSpace(q) != "" {
			fmt.Printf("    qdisc:  %s\n", strings.ReplaceAll(strings.TrimSpace(q), "\n", "\n            "))
		}
		if strings.TrimSpace(f) != "" {
			fmt.Printf("    filter: %s\n", strings.ReplaceAll(strings.TrimSpace(f), "\n", "\n            "))
		}
	}
	return nil
}

// chaosParseParams turns key=value args (delay=100ms jitter=20ms loss=5%) into params.
func chaosParseParams(args []string) (chaosNetemParams, error) {
	p := chaosNetemParams{}
	for _, a := range args {
		kv := strings.SplitN(a, "=", 2)
		if len(kv) != 2 {
			return p, fmt.Errorf("expected key=value, got %q", a)
		}
		key, val := strings.ToLower(strings.TrimSpace(kv[0])), strings.TrimSpace(kv[1])
		switch key {
		case "delay", "latency":
			p.Delay = val
		case "jitter":
			p.Jitter = val
		case "loss":
			p.Loss = val
		case "corr", "losscorr", "burst":
			p.LossCorr = val
		case "corrupt":
			p.Corrupt = val
		case "duplicate", "dup":
			p.Duplicate = val
		case "reorder":
			p.Reorder = val
		case "rate", "bw", "bandwidth":
			p.Rate = val
		default:
			return p, fmt.Errorf("unknown parameter %q (use delay=, jitter=, loss=, corr=, corrupt=, dup=, reorder=, rate=)", key)
		}
	}
	p.normalize()
	if err := p.validate(); err != nil {
		return p, err
	}
	return p, nil
}
