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
//   - root qdisc on eth0: prio with a priomap that sends ALL default traffic to
//     band 1:3, so the netem band (1:1) stays empty until a filter targets it.
//   - netem qdisc on band 1:1 carrying the delay/jitter/loss profile.
//   - u32 filters matching a specific peer IP redirect only that peer's traffic
//     into band 1:1. Everything else flows through the untouched band.
//
// A link A<->B is made symmetric by shaping eth0 egress on BOTH containers and
// filtering the peer's IP on each (egress dst-match is the meaningful direction;
// src is added belt-and-suspenders).

import (
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	anydbver_common "github.com/zelmario/anydbver/pkg/common"
	"github.com/zelmario/anydbver/pkg/runtools"
)

const (
	chaosNetemBand     = "1:1"
	chaosNetemHandle   = "10:"
	chaosDefaultTTLSec = 3600 // dead-man switch: auto-clear after 1h
	// chaosTcImageDefault is a tiny image whose entrypoint is tc. Overridable via
	// ANYDBVER_TC_IMAGE. (Folding iproute-tc into the anydbver ansible image at a
	// future release would drop this external dependency.)
	chaosTcImageDefault = "gaiadocker/iproute2"
	// chaosPingImageDefault carries `ping` (the el9 nodes don't). Overridable via
	// ANYDBVER_PING_IMAGE. busybox is tiny and has a working ping.
	chaosPingImageDefault = "busybox"
)

// priomap routes every TOS value to band index 2 (class 1:3), leaving band 0
// (class 1:1, where netem lives) empty until a filter redirects traffic into it.
var chaosPriomap = []string{"2", "2", "2", "2", "2", "2", "2", "2", "2", "2", "2", "2", "2", "2", "2", "2"}

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

// chaosMeasureRTT pings targetIP from inside container's network namespace and
// returns the average round-trip time (ms) and packet-loss %. A 100% loss result
// means the path is severed (partition) — rtt is then meaningless.
func chaosMeasureRTT(logger *log.Logger, container, targetIP string) (float64, int) {
	args := []string{"docker", "run", "--rm",
		"--net=container:" + container, "--cap-add=NET_RAW",
		chaosPingImage(), "ping", "-c", "5", "-w", "8", targetIP}
	out, _ := runtools.RunGetOutput(logger, args, "ping failed", regexp.MustCompile(".*"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)

	loss := 100
	rtt := 0.0
	for _, line := range strings.Split(out, "\n") {
		if m := regexp.MustCompile(`(\d+)% packet loss`).FindStringSubmatch(line); m != nil {
			loss, _ = strconv.Atoi(m[1])
		}
		// busybox: "round-trip min/avg/max = 0.1/0.2/0.3 ms"; iputils: "rtt min/avg/max/mdev = ..."
		if strings.Contains(line, "min/avg/max") {
			if eq := strings.Index(line, "="); eq >= 0 {
				fields := strings.Fields(strings.TrimSpace(line[eq+1:]))
				if len(fields) > 0 {
					if parts := strings.Split(fields[0], "/"); len(parts) >= 2 {
						rtt, _ = strconv.ParseFloat(parts[1], 64)
					}
				}
			}
		}
	}
	return rtt, loss
}

// chaosTc runs a tc command inside the target container's network namespace via
// a throwaway helper container. The qdisc/filter it sets persists on the
// target's eth0 after the helper exits. `ignore` is a regexp of stderr to treat
// as success (use ".*" for best-effort deletes).
func chaosTc(logger *log.Logger, container string, ignore string, tcArgs ...string) (string, error) {
	args := []string{"docker", "run", "--rm",
		"--net=container:" + container, "--cap-add=NET_ADMIN",
		"--entrypoint", "tc", chaosTcImage()}
	args = append(args, tcArgs...)

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

// chaosSh runs a shell script inside the target container's network namespace
// via one throwaway helper container. Batching many tc commands into a single
// `docker run` is much faster than spawning a helper per command (each spawn
// costs hundreds of ms under Docker Desktop).
func chaosSh(logger *log.Logger, container, script string) (string, error) {
	args := []string{"docker", "run", "--rm",
		"--net=container:" + container, "--cap-add=NET_ADMIN",
		"--entrypoint", "sh", chaosTcImage(), "-c", script}
	return runtools.RunGetOutput(logger, args, "tc batch failed", regexp.MustCompile("ignore this"), false, map[string]string{}, runtools.COMMAND_TIMEOUT)
}

// chaosShapeContainer arms eth0 (prio root + netem band) and filters the given
// peer IPs into the netem band — all in a single helper-container invocation.
// When reset is true the existing root qdisc is dropped first, so the container
// is rebuilt cleanly from the full peer set (used by the dashboard reconcile);
// when false the prio root is added idempotently so repeated calls accumulate
// filters (used by the one-shot CLI).
func chaosShapeContainer(logger *log.Logger, container string, params chaosNetemParams, peerIPs []string, reset bool) error {
	var b strings.Builder
	if reset {
		b.WriteString("tc qdisc del dev eth0 root 2>/dev/null; ")
	}
	prio := strings.Join(append([]string{"tc", "qdisc", "add", "dev", "eth0", "root", "handle", "1:", "prio", "priomap"}, chaosPriomap...), " ")
	fmt.Fprintf(&b, "%s 2>/dev/null; ", prio) // ignore "RTNETLINK answers: File exists"
	netem := strings.Join(append([]string{"tc", "qdisc", "replace", "dev", "eth0", "parent", chaosNetemBand, "handle", chaosNetemHandle}, params.netemArgs()...), " ")
	fmt.Fprintf(&b, "%s; ", netem)
	for _, ip := range peerIPs {
		for _, dir := range []string{"dst", "src"} {
			fmt.Fprintf(&b, "tc filter add dev eth0 protocol ip parent 1:0 prio 1 u32 match ip %s %s/32 flowid %s; ", dir, ip, chaosNetemBand)
		}
	}
	if _, err := chaosSh(logger, container, b.String()); err != nil {
		return fmt.Errorf("shaping %s: %w", container, err)
	}
	return nil
}

// chaosClearContainer removes the whole root qdisc (netem + filters) from eth0.
func chaosClearContainer(logger *log.Logger, container string) {
	chaosTc(logger, container, ".*", "qdisc", "del", "dev", "eth0", "root")
}

// chaosScheduleDeadMan spawns a detached process that clears the given
// containers after ttlSec seconds, so a forgotten fault can't cripple a cluster
// forever.
func chaosScheduleDeadMan(logger *log.Logger, containers []string, ttlSec int) {
	img := chaosTcImage()
	cmds := []string{fmt.Sprintf("sleep %d", ttlSec)}
	for _, c := range containers {
		cmds = append(cmds, fmt.Sprintf(
			"docker run --rm --net=container:%s --cap-add=NET_ADMIN --entrypoint tc %s qdisc del dev eth0 root 2>/dev/null",
			c, img))
	}
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

// chaosDegradeLink applies a symmetric netem profile to the A<->B link.
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

	// Shape both endpoints concurrently (each is one helper-container run).
	errs := make(chan error, 2)
	go func() { errs <- chaosShapeContainer(logger, a.Container, params, []string{b.IP}, false) }()
	go func() { errs <- chaosShapeContainer(logger, b.Container, params, []string{a.IP}, false) }()
	if e := <-errs; e != nil {
		<-errs
		return e
	}
	if e := <-errs; e != nil {
		return e
	}

	logger.Printf("chaos: degraded link %s(%s) <-> %s(%s): %s",
		a.Node, a.IP, b.Node, b.IP, params.human())
	if ttlSec > 0 {
		chaosScheduleDeadMan(logger, []string{a.Container, b.Container}, ttlSec)
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

// chaosClearAll removes all tc shaping from every node's eth0.
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
