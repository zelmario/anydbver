package main

// Chaos dashboard: a small self-contained HTTP server (no web framework) that
// serves an embedded SVG topology page and drives the chaos engine via JSON.
// State lives in-memory in this process; link changes are applied by clearing
// all shaping and re-applying the current set (simple + always consistent for
// the small clusters anydbver builds). Faults are cleared on graceful shutdown,
// and an inactivity dead-man timer clears them if the operator walks away.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed chaos_ui.html
var chaosUIHTML string

type chaosLinkState struct {
	A      string           `json:"a"`
	B      string           `json:"b"`
	Params chaosNetemParams `json:"-"`
}

type chaosServer struct {
	logger    *log.Logger
	provider  string
	namespace string
	ttl       int

	mu      sync.Mutex
	links   map[string]chaosLinkState // key = sorted "a|b"
	deadman *time.Timer

	fluxInterval int                   // seconds; 0 = off. Re-rolls ranges on a timer.
	fluxStop     chan struct{}         // closed to stop the flux ticker
	flaps        map[string]*chaosFlap // link key -> running flap
	monkey       chaosMonkey           // random node-kill loop
}

type chaosFlap struct {
	a, b      string
	downSec   int
	upSec     int
	phaseDown bool // true while the link is currently partitioned
	stop      chan struct{}
}

type chaosMonkey struct {
	enabled  bool
	mode     string // pause | kill
	interval int
	stop     chan struct{}
}

func chaosLinkKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}

// effectiveLinks merges the user's baseline degrade (s.links) with any active
// flap overlay: while a link is in its "down" phase it is fully partitioned;
// while "up" it falls back to the baseline degrade (or fully healed if there is
// none). This lets a link be degraded AND flapped at the same time. Caller holds mu.
func (s *chaosServer) effectiveLinks() []chaosLinkState {
	out := map[string]chaosLinkState{}
	for k, l := range s.links {
		out[k] = l
	}
	for k, f := range s.flaps {
		if f == nil {
			continue
		}
		if f.phaseDown {
			out[k] = chaosLinkState{A: f.a, B: f.b, Params: chaosNetemParams{Loss: "100%"}}
		} else if _, ok := s.links[k]; !ok {
			delete(out, k) // flap-only link, healed phase → no shaping
		}
	}
	res := make([]chaosLinkState, 0, len(out))
	for _, l := range out {
		res = append(res, l)
	}
	return res
}

// reconcile rebuilds all shaping from the current link set. Caller holds mu.
// It lists nodes once, clears every node in parallel, then applies one batched
// helper-container run per shaped container (also in parallel) — keeping the UI
// snappy regardless of how many links are active.
func (s *chaosServer) reconcile() {
	nodes, err := chaosListNodes(s.logger, s.provider, s.namespace)
	if err != nil {
		s.logger.Printf("chaos-ui: reconcile: %v", err)
		return
	}
	byName := map[string]chaosNode{}
	for _, n := range nodes {
		byName[n.Node] = n
	}

	// Aggregate per-container: every peer IP it must filter, plus the netem
	// profile (one band per container, so the last link's params win — matches
	// the single-band model; mixing distinct profiles on one node is a known v2).
	type agg struct {
		params chaosNetemParams
		peers  []string
	}
	perContainer := map[string]*agg{}
	add := func(self, peer chaosNode, p chaosNetemParams) {
		if self.IP == "" || peer.IP == "" {
			return
		}
		a := perContainer[self.Container]
		if a == nil {
			a = &agg{}
			perContainer[self.Container] = a
		}
		a.params = p
		a.peers = append(a.peers, peer.IP)
	}
	for _, l := range s.effectiveLinks() {
		a, b := byName[l.A], byName[l.B]
		add(a, b, l.Params)
		add(b, a, l.Params)
	}

	// One parallel phase: each reachable node is either rebuilt cleanly (shaped,
	// reset=true fuses clear+arm+filters into one helper run) or just cleared.
	var wg sync.WaitGroup
	for _, n := range nodes {
		if n.State != "running" && n.State != "paused" {
			continue
		}
		wg.Add(1)
		go func(c string) {
			defer wg.Done()
			if a := perContainer[c]; a != nil {
				if err := chaosShapeContainer(s.logger, c, a.params, a.peers, true); err != nil {
					s.logger.Printf("chaos-ui: shape %s: %v", c, err)
				}
			} else {
				chaosClearContainer(s.logger, c)
			}
		}(n.Container)
	}
	wg.Wait()

	s.armDeadman()
}

// armDeadman (re)starts the inactivity timer that clears everything after ttl
// seconds with no further changes. Caller holds mu.
func (s *chaosServer) armDeadman() {
	if s.ttl <= 0 {
		return
	}
	if s.deadman != nil {
		s.deadman.Stop()
	}
	s.deadman = time.AfterFunc(time.Duration(s.ttl)*time.Second, func() {
		s.logger.Printf("chaos-ui: inactivity dead-man fired — clearing all faults")
		s.mu.Lock()
		s.links = map[string]chaosLinkState{}
		chaosClearAll(s.logger, s.provider, s.namespace)
		s.mu.Unlock()
	})
}

// --- HTTP handlers -----------------------------------------------------------

type topoNode struct {
	Node   string `json:"node"`
	IP     string `json:"ip"`
	State  string `json:"state"`
	Shaped bool   `json:"shaped"`
}

type topoLink struct {
	A         string `json:"a"`
	B         string `json:"b"`
	Delay     string `json:"delay"`
	Jitter    string `json:"jitter"`
	Loss      string `json:"loss"`
	LossCorr  string `json:"losscorr"`
	Corrupt   string `json:"corrupt"`
	Duplicate string `json:"duplicate"`
	Reorder   string `json:"reorder"`
	Rate      string `json:"rate"`
	Partition bool   `json:"partition"`
	Flapping  bool   `json:"flapping"`
}

func (s *chaosServer) handleTopology(w http.ResponseWriter, r *http.Request) {
	nodes, err := chaosListNodes(s.logger, s.provider, s.namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	shaped := map[string]bool{}
	links := []topoLink{}
	for k, l := range s.links {
		shaped[l.A] = true
		shaped[l.B] = true
		p := l.Params
		links = append(links, topoLink{
			A: l.A, B: l.B,
			Delay: p.Delay, Jitter: p.Jitter, Loss: p.Loss, LossCorr: p.LossCorr,
			Corrupt: p.Corrupt, Duplicate: p.Duplicate, Reorder: p.Reorder, Rate: p.Rate,
			Partition: p.Delay == "" && p.Loss == "100%",
			Flapping:  s.flaps[k] != nil,
		})
	}
	// flapping links may be in the "up" (healed) phase with no current shaping —
	// surface them so the UI can still show the pair as flapping.
	for k, f := range s.flaps {
		if _, ok := s.links[k]; ok || f == nil {
			continue
		}
		a, b, _ := splitLinkKey(k)
		links = append(links, topoLink{A: a, B: b, Flapping: true})
	}
	flux := s.fluxInterval
	monkey := map[string]any{"enabled": s.monkey.enabled, "mode": s.monkey.mode, "interval": s.monkey.interval}
	s.mu.Unlock()

	tn := make([]topoNode, 0, len(nodes))
	for _, n := range nodes {
		tn = append(tn, topoNode{Node: n.Node, IP: n.IP, State: n.State, Shaped: shaped[n.Node]})
	}
	writeJSON(w, map[string]any{"nodes": tn, "links": links, "flux": flux, "monkey": monkey})
}

type chaosLinkReq struct {
	A         string `json:"a"`
	B         string `json:"b"`
	Delay     string `json:"delay"`
	Jitter    string `json:"jitter"`
	Loss      string `json:"loss"`
	LossCorr  string `json:"losscorr"`
	Corrupt   string `json:"corrupt"`
	Duplicate string `json:"duplicate"`
	Reorder   string `json:"reorder"`
	Rate      string `json:"rate"`
	Partition bool   `json:"partition"`
	Enabled   bool   `json:"enabled"`
}

// toParams builds a normalized netem profile from the request (sans partition).
func (req chaosLinkReq) toParams() chaosNetemParams {
	p := chaosNetemParams{
		Delay: req.Delay, Jitter: req.Jitter, Loss: req.Loss, LossCorr: req.LossCorr,
		Corrupt: req.Corrupt, Duplicate: req.Duplicate, Reorder: req.Reorder, Rate: req.Rate,
	}
	p.normalize()
	return p
}

// validate checks node names and (for a degrade) the fault values.
func (req chaosLinkReq) validate() error {
	if req.A == "" || req.B == "" || req.A == req.B {
		return fmt.Errorf("need two distinct nodes")
	}
	if req.Enabled && !req.Partition {
		return req.toParams().validate()
	}
	return nil
}

// applyLinkLocked mutates s.links for one (already-validated) request without
// reconciling. Caller holds mu.
func (s *chaosServer) applyLinkLocked(req chaosLinkReq) {
	key := chaosLinkKey(req.A, req.B)
	if !req.Enabled {
		delete(s.links, key)
		return
	}
	params := req.toParams()
	if req.Partition {
		params = chaosNetemParams{Loss: "100%"}
	}
	s.links[key] = chaosLinkState{A: req.A, B: req.B, Params: params}
}

func (s *chaosServer) handleLink(w http.ResponseWriter, r *http.Request) {
	var req chaosLinkReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := req.validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.applyLinkLocked(req)
	s.reconcile()
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// handleLinks applies many link ops in one shot, reconciling only once — used by
// the dashboard's multi-select so N links don't trigger N reconciles.
func (s *chaosServer) handleLinks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ops []chaosLinkReq `json:"ops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	for _, op := range req.Ops { // validate all before applying any
		if err := op.validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	s.mu.Lock()
	for _, op := range req.Ops {
		s.applyLinkLocked(op)
	}
	s.reconcile()
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *chaosServer) handleMeasure(w http.ResponseWriter, r *http.Request) {
	var req struct {
		A string `json:"a"`
		B string `json:"b"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	nodes, err := chaosListNodes(s.logger, s.provider, s.namespace)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a, err := chaosResolveNode(s.logger, s.namespace, nodes, req.A)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	b, err := chaosResolveNode(s.logger, s.namespace, nodes, req.B)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if b.IP == "" {
		http.Error(w, b.Node+" has no IP", http.StatusConflict)
		return
	}
	rtt, loss := chaosMeasureRTT(s.logger, a.Container, b.IP)
	writeJSON(w, map[string]any{"rtt": rtt, "oneway": rtt / 2, "loss": loss})
}

func (s *chaosServer) handleNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Node   string `json:"node"`
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := chaosNodeAction(s.logger, s.provider, s.namespace, req.Node, req.Action); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *chaosServer) handleClear(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.links = map[string]chaosLinkState{}
	chaosClearAll(s.logger, s.provider, s.namespace)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func splitLinkKey(k string) (string, string, bool) {
	i := strings.Index(k, "|")
	if i < 0 {
		return "", "", false
	}
	return k[:i], k[i+1:], true
}

// --- flux: periodically re-roll ranged params so conditions drift -----------

// setFlux (re)configures the flux ticker. interval<=0 stops it.
func (s *chaosServer) setFlux(interval int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fluxStop != nil {
		close(s.fluxStop)
		s.fluxStop = nil
	}
	s.fluxInterval = interval
	if interval <= 0 {
		return
	}
	stop := make(chan struct{})
	s.fluxStop = stop
	go func() {
		t := time.NewTicker(time.Duration(interval) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.mu.Lock()
				if len(s.links) > 0 {
					s.reconcile() // chaosShapeContainer re-rolls every range
				}
				s.mu.Unlock()
			}
		}
	}()
}

func (s *chaosServer) handleFlux(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Interval int `json:"interval"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.setFlux(req.Interval)
	s.logger.Printf("chaos-ui: flux %s", onOff(req.Interval > 0, req.Interval))
	writeJSON(w, map[string]any{"ok": true})
}

// --- flap: cycle a link between partitioned (down) and healed (up) ----------

func (s *chaosServer) handleFlap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		A       string `json:"a"`
		B       string `json:"b"`
		Down    int    `json:"down"`
		Up      int    `json:"up"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.A == "" || req.B == "" || req.A == req.B {
		http.Error(w, "need two distinct nodes", http.StatusBadRequest)
		return
	}
	key := chaosLinkKey(req.A, req.B)

	s.mu.Lock()
	if f := s.flaps[key]; f != nil {
		close(f.stop)
		delete(s.flaps, key)
	}
	if !req.Enabled {
		// stop flapping but KEEP any baseline degrade the user set on this link.
		s.reconcile()
		s.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true})
		return
	}
	down, up := req.Down, req.Up
	if down <= 0 {
		down = 5
	}
	if up <= 0 {
		up = 10
	}
	stop := make(chan struct{})
	s.flaps[key] = &chaosFlap{a: req.A, b: req.B, downSec: down, upSec: up, stop: stop}
	s.mu.Unlock()

	go s.flapLoop(key, down, up, stop)
	s.logger.Printf("chaos-ui: flap %s<->%s down=%ds up=%ds", req.A, req.B, down, up)
	writeJSON(w, map[string]any{"ok": true})
}

// flapLoop flips the flap's phase and reconciles; it never touches s.links, so a
// baseline degrade on the same link survives across the up/down cycle.
func (s *chaosServer) flapLoop(key string, down, up int, stop chan struct{}) {
	setPhase := func(downPhase bool) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		f := s.flaps[key]
		if f == nil || f.stop != stop {
			return false // superseded/cancelled
		}
		f.phaseDown = downPhase
		s.reconcile()
		return true
	}
	for {
		if !setPhase(true) { // DOWN — partition
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(down) * time.Second):
		}
		if !setPhase(false) { // UP — fall back to baseline degrade (or healed)
			return
		}
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(up) * time.Second):
		}
	}
}

// --- chaos monkey: periodically disrupt a random node -----------------------

func (s *chaosServer) handleMonkey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  bool   `json:"enabled"`
		Mode     string `json:"mode"`
		Interval int    `json:"interval"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s.setMonkey(req.Enabled, req.Interval, req.Mode)
	writeJSON(w, map[string]any{"ok": true})
}

func (s *chaosServer) setMonkey(enabled bool, interval int, mode string) {
	s.mu.Lock()
	if s.monkey.stop != nil {
		close(s.monkey.stop)
	}
	s.monkey = chaosMonkey{}
	if !enabled {
		s.mu.Unlock()
		s.logger.Printf("chaos-ui: monkey off")
		return
	}
	if interval <= 0 {
		interval = 15
	}
	if mode != "kill" {
		mode = "pause"
	}
	stop := make(chan struct{})
	s.monkey = chaosMonkey{enabled: true, mode: mode, interval: interval, stop: stop}
	s.mu.Unlock()
	go s.monkeyLoop(mode, interval, stop)
	s.logger.Printf("chaos-ui: monkey on (%s every %ds)", mode, interval)
}

func (s *chaosServer) monkeyLoop(mode string, interval int, stop chan struct{}) {
	recover := "unpause"
	if mode == "kill" {
		recover = "start"
	}
	for {
		select {
		case <-stop:
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
		nodes, err := chaosListNodes(s.logger, s.provider, s.namespace)
		if err != nil {
			continue
		}
		running := []chaosNode{}
		for _, n := range nodes {
			if n.State == "running" {
				running = append(running, n)
			}
		}
		if len(running) < 2 { // never take down the last node standing
			continue
		}
		victim := running[rand.Intn(len(running))]
		_ = chaosNodeAction(s.logger, s.provider, s.namespace, victim.Node, mode)
		// hold the disruption for ~half the interval, then recover.
		select {
		case <-stop:
			_ = chaosNodeAction(s.logger, s.provider, s.namespace, victim.Node, recover)
			return
		case <-time.After(time.Duration(interval) * time.Second / 2):
		}
		_ = chaosNodeAction(s.logger, s.provider, s.namespace, victim.Node, recover)
	}
}

// stopAll halts every background loop (flux, flaps, monkey). Used on shutdown.
func (s *chaosServer) stopAll() {
	s.setFlux(0)
	s.setMonkey(false, 0, "")
	s.mu.Lock()
	for k, f := range s.flaps {
		close(f.stop)
		delete(s.flaps, k)
	}
	s.mu.Unlock()
}

func onOff(on bool, n int) string {
	if on {
		return fmt.Sprintf("on (every %ds)", n)
	}
	return "off"
}

// runChaosUI starts the dashboard server and blocks until interrupted.
func runChaosUI(logger *log.Logger, provider, namespace string, port, ttl, flux int) {
	if provider != "docker" {
		logger.Fatalf("chaos: the dashboard currently supports only the docker provider (got %q)", provider)
	}
	// Fail fast with a helpful message if there's nothing to act on.
	if _, err := chaosListNodes(logger, provider, namespace); err != nil {
		logger.Fatalf("chaos: %v", err)
	}

	s := &chaosServer{
		logger: logger, provider: provider, namespace: namespace, ttl: ttl,
		links: map[string]chaosLinkState{},
		flaps: map[string]*chaosFlap{},
	}
	if flux > 0 {
		s.setFlux(flux)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, chaosUIHTML)
	})
	mux.HandleFunc("/api/topology", s.handleTopology)
	mux.HandleFunc("/api/link", s.handleLink)
	mux.HandleFunc("/api/links", s.handleLinks)
	mux.HandleFunc("/api/node", s.handleNode)
	mux.HandleFunc("/api/measure", s.handleMeasure)
	mux.HandleFunc("/api/clear", s.handleClear)
	mux.HandleFunc("/api/flux", s.handleFlux)
	mux.HandleFunc("/api/flap", s.handleFlap)
	mux.HandleFunc("/api/monkey", s.handleMonkey)

	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := &http.Server{Addr: addr, Handler: mux}
	url := "http://" + addr + "/"

	ns := namespace
	if ns == "" {
		ns = "(default)"
	}
	logger.Printf("chaos dashboard for namespace %s at %s", ns, url)
	logger.Printf("open it in your browser; press Ctrl-C to stop and clear all faults")
	chaosOpenBrowser(logger, url)

	// Clear faults on Ctrl-C / SIGTERM, then exit.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Printf("chaos: shutting down, clearing all faults")
		s.stopAll()
		s.mu.Lock()
		s.links = map[string]chaosLinkState{}
		chaosClearAll(s.logger, s.provider, s.namespace)
		s.mu.Unlock()
		_ = srv.Close()
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("chaos: server error: %v", err)
	}
}

// chaosOpenBrowser best-effort opens the dashboard URL; failure is non-fatal (the URL is always printed)
// (the URL is always printed). On WSL it uses the Windows browser.
func chaosOpenBrowser(logger *log.Logger, url string) {
	var candidates [][]string
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
		candidates = append(candidates, []string{"wslview", url}, []string{"explorer.exe", url})
	}
	candidates = append(candidates, []string{"xdg-open", url}, []string{"open", url})
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err != nil {
			continue
		}
		if err := exec.Command(c[0], c[1:]...).Start(); err == nil {
			return
		}
	}
}
