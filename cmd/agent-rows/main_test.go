// Offline tests: a fake herdr socket server, a fake gh script, and real
// throwaway git repos. Each scenario runs in its own sandbox (socket, state
// dir, gh log); state persists between sweeps in one sandbox, as it does
// between real events.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type world struct {
	Agents     []map[string]any `json:"agents"`
	Workspaces []map[string]any `json:"workspaces"`
	Panes      []map[string]any `json:"panes"`
	noAgents   bool
}

type report struct {
	pane  string
	set   map[string]string
	clear []string
}

type sandbox struct {
	t        *testing.T
	dir      string
	base     string // the plugin's state dir, shared by sessions
	stateDir string // this sandbox's session under base
	sock     string
	ghLog    string
	mu       sync.Mutex
	world    *world
	reports  []report
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	// Short path: unix socket paths are capped at ~104 bytes on macOS.
	dir, err := os.MkdirTemp("", "ar")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sb := &sandbox{t: t, dir: dir, base: filepath.Join(dir, "state"), sock: filepath.Join(dir, "h.sock"), ghLog: filepath.Join(dir, "gh.log")}
	sb.stateDir = filepath.Join(sb.base, sessionDir(sb.sock))
	os.MkdirAll(sb.stateDir, 0o700)
	ln, err := net.Listen("unix", sb.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go sb.serve(conn)
		}
	}()
	return sb
}

// serve answers one request per connection, like herdr.
func (sb *sandbox) serve(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	json.Unmarshal(line, &req)
	sb.mu.Lock()
	defer sb.mu.Unlock()
	var result any
	switch req.Method {
	case "session.snapshot":
		snap := map[string]any{"workspaces": sb.world.Workspaces, "panes": sb.world.Panes}
		if !sb.world.noAgents {
			snap["agents"] = sb.world.Agents
		}
		result = map[string]any{"type": "session_snapshot", "snapshot": snap}
	case "pane.report_metadata":
		var p struct {
			PaneID string             `json:"pane_id"`
			Source string             `json:"source"`
			Tokens map[string]*string `json:"tokens"`
		}
		json.Unmarshal(req.Params, &p)
		if p.Source != metadataSource {
			sb.t.Errorf("source = %q", p.Source)
		}
		r := report{pane: p.PaneID, set: map[string]string{}}
		for k, v := range p.Tokens {
			if v == nil {
				r.clear = append(r.clear, k)
			} else {
				r.set[k] = *v
			}
		}
		sb.reports = append(sb.reports, r)
		result = map[string]any{"type": "ok"}
	default:
		fmt.Fprintf(conn, `{"id":%q,"error":{"code":"unknown_method","message":"x"}}`+"\n", req.ID)
		return
	}
	b, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
	conn.Write(append(b, '\n'))
}

type runOpts struct {
	prs        map[string]string // branch → gh JSON output
	ghSleep    int               // seconds
	gh         string            // override gh path
	noStateDir bool
	stateDir   string // override the plugin's state dir (sb.base)
	dry        bool
}

type result struct {
	reports []report
	gh      []string
	state   state
	stdout  string
	stderr  string
	elapsed time.Duration
	lockOn  bool
}

// fakeGh writes a gh stand-in with this run's answers baked in. It answers
// `gh pr list --head=<branch>` with the PR list for that branch; a single
// PR object in o.prs is served as a one-item list. Like the real gh,
// `gh pr view <n>` with a numeric argument shows PR n.
func (sb *sandbox) fakeGh(o runOpts) string {
	var cases strings.Builder
	for branch, out := range o.prs {
		if !strings.HasPrefix(out, "[") {
			out = "[" + out + "]"
		}
		fmt.Fprintf(&cases, "  %q) printf '%%s' %q ;;\n", branch, out)
	}
	sleep := ""
	if o.ghSleep > 0 {
		sleep = fmt.Sprintf("sleep %d\n", o.ghSleep)
	}
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\n%sif [ \"$2\" = view ]; then for a; do last=\"$a\"; done; case \"$last\" in [0-9]*|\\#[0-9]*) printf '{\"number\":%%s,\"state\":\"OPEN\"}' \"${last#\\#}\"; exit 0 ;; esac; exit 1; fi\nfor a; do case \"$a\" in --head=*) branch=\"${a#--head=}\" ;; esac; done\ncase \"$branch\" in\n%s  *) printf '[]' ;;\nesac\n", sb.ghLog, sleep, cases.String())
	p := filepath.Join(sb.dir, "gh")
	os.WriteFile(p, []byte(script), 0o755)
	return p
}

func (sb *sandbox) run(w *world, o runOpts) result {
	sb.t.Helper()
	sb.mu.Lock()
	sb.world = w
	sb.reports = nil
	sb.mu.Unlock()
	os.WriteFile(sb.ghLog, nil, 0o644)
	gh := o.gh
	if gh == "" {
		gh = sb.fakeGh(o)
	}
	var stdout, stderr bytes.Buffer
	c := &config{dry: o.dry, socket: sb.sock, stateDir: sb.base, gh: gh, stdout: &stdout, stderr: &stderr}
	if o.noStateDir {
		c.stateDir = ""
	}
	if o.stateDir != "" {
		c.stateDir = o.stateDir
	}
	t0 := time.Now()
	run(c)
	r := result{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(t0)}
	sb.mu.Lock()
	r.reports = sb.reports
	sb.mu.Unlock()
	if b, err := os.ReadFile(sb.ghLog); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if l != "" {
				r.gh = append(r.gh, l)
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(sb.stateDir, "state.json")); err == nil {
		json.Unmarshal(b, &r.state)
	}
	_, err := os.Stat(filepath.Join(sb.stateDir, ".lock"))
	r.lockOn = err == nil
	return r
}

// apply folds a sweep's reports into the world, as herdr would.
func apply(w *world, reps []report) {
	for _, r := range reps {
		for _, a := range w.Agents {
			if a["pane_id"] != r.pane {
				continue
			}
			toks, _ := a["tokens"].(map[string]any)
			if toks == nil {
				toks = map[string]any{}
			}
			for k, v := range r.set {
				toks[k] = v
			}
			for _, k := range r.clear {
				delete(toks, k)
			}
			a["tokens"] = toks
		}
	}
}

func byPane(reps []report) map[string]report {
	m := map[string]report{}
	for _, r := range reps {
		m[r.pane] = r
	}
	return m
}

func agent(pane, ws string, extra map[string]any) map[string]any {
	a := map[string]any{
		"agent": "claude", "agent_status": "idle", "pane_id": pane,
		"tab_id": ws + ":t1", "workspace_id": ws, "terminal_id": "term_" + pane, "cwd": "/nonexistent",
	}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func ws(id, label string) map[string]any { return map[string]any{"workspace_id": id, "label": label} }

func roles(kv ...string) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

func eqSet(t *testing.T, got report, want map[string]string, msg string) {
	t.Helper()
	if len(got.set) != len(want) {
		t.Errorf("%s: set = %v, want %v", msg, got.set, want)
		return
	}
	for k, v := range want {
		if got.set[k] != v {
			t.Errorf("%s: set = %v, want %v", msg, got.set, want)
			return
		}
	}
}

// ---- git fixtures -------------------------------------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "init.defaultBranch=main"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// repo makes a git repo on `branch`, optionally with origin/HEAD → def.
func repo(t *testing.T, root, name, branch, def string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	os.MkdirAll(dir, 0o755)
	git(t, dir, "init", "-q", "-b", branch)
	if def != "" {
		git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+def)
	}
	return dir
}

var hhmm = regexp.MustCompile(`^(done|idle|blocked) \d\d:\d\d$`)

// ---- scenarios -------------------------------------------------------------------

func TestSoloAgent(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{
		Agents: []map[string]any{
			agent("w1:p1", "w1", map[string]any{"terminal_title_stripped": "notes"}),
			agent("w2:p1", "w2", map[string]any{"terminal_title_stripped": "Refactor the parser", "agent_status": "working"}),
		},
		Workspaces: []map[string]any{ws("w1", "notes"), ws("w2", "cli")},
	}, runOpts{})
	m := byPane(r.reports)
	eqSet(t, m["w1:p1"], map[string]string{"ar_name": "notes", "ar_state": "idle"}, "w1")
	eqSet(t, m["w2:p1"], map[string]string{"ar_name": "cli", "ar_title": "Refactor the parser"}, "w2")
}

func TestTitleDedupIgnoresCaseAndSpace(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{
		Agents:     []map[string]any{agent("w1:p1", "w1", map[string]any{"terminal_title_stripped": " Notes "})},
		Workspaces: []map[string]any{ws("w1", "notes")},
	}, runOpts{})
	if _, ok := byPane(r.reports)["w1:p1"].set["ar_title"]; ok {
		t.Error("title should be de-duplicated")
	}
}

func TestStampAndDoneToIdleKeepsTime(t *testing.T) {
	sb := newSandbox(t)
	w := &world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"agent_status": "working"})}, Workspaces: []map[string]any{ws("w1", "notes")}}
	apply(w, sb.run(w, runOpts{}).reports)
	w.Agents[0]["agent_status"] = "done"
	r2 := sb.run(w, runOpts{})
	done := byPane(r2.reports)["w1:p1"].set["ar_state"]
	if !hhmm.MatchString(done) || !strings.HasPrefix(done, "done ") {
		t.Fatalf("ar_state = %q (%s)", done, r2.stderr)
	}
	apply(w, r2.reports)
	w.Agents[0]["agent_status"] = "idle"
	r3 := sb.run(w, runOpts{})
	if got := byPane(r3.reports)["w1:p1"].set["ar_state"]; got != strings.Replace(done, "done", "idle", 1) {
		t.Errorf("idle ar_state = %q, want time kept from %q", got, done)
	}
}

func TestWorkingClearsState(t *testing.T) {
	sb := newSandbox(t)
	w := &world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "notes")}}
	apply(w, sb.run(w, runOpts{}).reports)
	w.Agents[0]["agent_status"] = "working"
	r := sb.run(w, runOpts{})
	if c := byPane(r.reports)["w1:p1"].clear; len(c) != 1 || c[0] != "ar_state" {
		t.Errorf("clear = %v", c)
	}
}

func TestUnchangedRowsNoReport(t *testing.T) {
	sb := newSandbox(t)
	w := &world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"terminal_title_stripped": strings.Repeat("x", 120)})}, Workspaces: []map[string]any{ws("w1", "notes")}}
	r1 := sb.run(w, runOpts{})
	if n := len([]rune(byPane(r1.reports)["w1:p1"].set["ar_title"])); n != 80 {
		t.Errorf("title len = %d, want 80 (capped like herdr)", n)
	}
	apply(w, r1.reports)
	if r2 := sb.run(w, runOpts{}); len(r2.reports) != 0 {
		t.Errorf("reports = %v", r2.reports)
	}
}

func TestLeadAndWorkers(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{
		Agents: []map[string]any{
			agent("w1:p1", "w1", map[string]any{"tokens": roles("role", "lead"), "terminal_title_stripped": "lead-title"}),
			agent("w1:p2", "w1", map[string]any{"name": "wide-layout", "tokens": roles("role", "worker", "lead", "w1:p1"), "agent_status": "done"}),
			agent("w1:p3", "w1", map[string]any{"tokens": roles("role", "worker", "lead", "w1:p1"), "agent_status": "blocked"}),
			agent("w1:p4", "w1", map[string]any{"tokens": roles("role", "worker", "lead", "w1:p1"), "agent_status": "working", "terminal_title_stripped": "t"}),
		},
		Workspaces: []map[string]any{ws("w1", "notes")},
		Panes:      []map[string]any{{"pane_id": "w1:p3", "label": "abc-migration"}},
	}, runOpts{})
	m := byPane(r.reports)
	eqSet(t, m["w1:p1"], map[string]string{"ar_name": "notes", "ar_state": "lead · 3 workers, 1 blocked"}, "lead")
	eqSet(t, m["w1:p2"], map[string]string{"ar_wname": workerIndent + "wide-layout", "ar_wstate": "done"}, "agent name")
	eqSet(t, m["w1:p3"], map[string]string{"ar_wname": workerIndent + "abc-migration", "ar_wstate": "blocked"}, "pane label fallback")
	eqSet(t, m["w1:p4"], map[string]string{"ar_wname": workerIndent + "t"}, "title fallback; working has no state text")
}

func TestLeadCounts(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{
		Agents: []map[string]any{
			agent("w1:p1", "w1", map[string]any{"tokens": roles("role", "lead")}),
			agent("w1:p2", "w1", map[string]any{"name": "a", "tokens": roles("role", "worker", "lead", "w1:p1")}),
			agent("w2:p1", "w2", map[string]any{"tokens": roles("role", "lead")}),
		},
		Workspaces: []map[string]any{ws("w1", "one"), ws("w2", "two")},
	}, runOpts{})
	m := byPane(r.reports)
	if got := m["w1:p1"].set["ar_state"]; got != "lead · 1 worker" {
		t.Errorf("one worker: %q", got)
	}
	if got := m["w2:p1"].set["ar_state"]; got != "lead" {
		t.Errorf("no workers: %q", got)
	}
}

func TestOrphanWorkerIsSolo(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{
		Agents: []map[string]any{
			agent("w1:p2", "w1", map[string]any{"name": "gone-lead", "tokens": roles("role", "worker", "lead", "w1:p1"), "terminal_title_stripped": "gone-lead"}),
			agent("w1:p3", "w1", map[string]any{"tokens": roles("role", "worker", "lead", "w1:p4")}),
			agent("w1:p4", "w1", map[string]any{"tokens": roles("role", "worker", "lead", "w1:p3")}),
		},
		Workspaces: []map[string]any{ws("w1", "notes")},
	}, runOpts{})
	m := byPane(r.reports)
	eqSet(t, m["w1:p2"], map[string]string{"ar_name": "notes", "ar_state": "idle", "ar_title": "gone-lead"}, "lead gone")
	for _, p := range []string{"w1:p3", "w1:p4"} {
		if m[p].set["ar_name"] != "notes" {
			t.Errorf("%s: lead is not a lead, got %v", p, m[p].set)
		}
	}
}

func TestWorkerBecomingSolo(t *testing.T) {
	sb := newSandbox(t)
	w := &world{
		Agents: []map[string]any{
			agent("w1:p1", "w1", map[string]any{"tokens": roles("role", "lead")}),
			agent("w1:p2", "w1", map[string]any{"name": "w", "tokens": roles("role", "worker", "lead", "w1:p1"), "terminal_title_stripped": "w"}),
		},
		Workspaces: []map[string]any{ws("w1", "notes")},
	}
	apply(w, sb.run(w, runOpts{}).reports)
	w.Agents = w.Agents[1:] // lead pane closed
	r := sb.run(w, runOpts{})
	got := byPane(r.reports)["w1:p2"]
	eqSet(t, got, map[string]string{"ar_name": "notes", "ar_state": "idle", "ar_title": "w"}, "orphaned")
	slices.Sort(got.clear)
	if fmt.Sprint(got.clear) != "[ar_wname ar_wstate]" {
		t.Errorf("clear = %v, want the worker tokens", got.clear)
	}
}

// A worker with no agent name, pane label or title is named by its pane id.
func TestWorkerNameFallsBackToPaneID(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{
		Agents: []map[string]any{
			agent("w1:p1", "w1", map[string]any{"tokens": roles("role", "lead")}),
			agent("w1:p2", "w1", map[string]any{"tokens": roles("role", "worker", "lead", "w1:p1")}),
		},
		Workspaces: []map[string]any{ws("w1", "notes")},
	}, runOpts{})
	if got := byPane(r.reports)["w1:p2"].set["ar_wname"]; got != workerIndent+"w1:p2" {
		t.Errorf("ar_name = %q", got)
	}
}

func TestBranchOnlyOffDefault(t *testing.T) {
	sb := newSandbox(t)
	onMain := repo(t, sb.dir, "a", "main", "main")
	onFeat := repo(t, sb.dir, "b", "feat/x", "main")
	developDefault := repo(t, sb.dir, "c", "develop", "develop")
	masterNoOrigin := repo(t, sb.dir, "d", "master", "")
	featNoOrigin := repo(t, sb.dir, "e", "feat/y", "")
	r := sb.run(&world{
		Agents: []map[string]any{
			agent("w1:p1", "w1", map[string]any{"cwd": onMain}),
			agent("w2:p1", "w2", map[string]any{"cwd": onFeat}),
			agent("w3:p1", "w3", map[string]any{"cwd": developDefault}),
			agent("w4:p1", "w4", map[string]any{"cwd": masterNoOrigin}),
			agent("w5:p1", "w5", map[string]any{"cwd": sb.dir}), // not a repo
			agent("w6:p1", "w6", map[string]any{"cwd": filepath.Join(featNoOrigin, ".")}),
		},
		Workspaces: []map[string]any{ws("w1", "w1"), ws("w2", "w2"), ws("w3", "w3"), ws("w4", "w4"), ws("w5", "w5"), ws("w6", "w6")},
	}, runOpts{})
	m := byPane(r.reports)
	for pane, want := range map[string]string{"w1:p1": "", "w2:p1": "feat/x", "w3:p1": "", "w4:p1": "", "w5:p1": "", "w6:p1": "feat/y"} {
		if got := m[pane].set["ar_git"]; got != want {
			t.Errorf("%s ar_git = %q, want %q (%s)", pane, got, want, r.stderr)
		}
	}
}

func TestPrLabels(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	b := repo(t, sb.dir, "b", "feat/b", "main")
	c := repo(t, sb.dir, "c", "feat/c", "main")
	r := sb.run(&world{
		Agents:     []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a}), agent("w2:p1", "w2", map[string]any{"cwd": b}), agent("w3:p1", "w3", map[string]any{"cwd": c})},
		Workspaces: []map[string]any{ws("w1", "a"), ws("w2", "b"), ws("w3", "c")},
	}, runOpts{prs: map[string]string{
		"feat/a": `{"number":42,"state":"OPEN"}`,
		"feat/b": `{"number":7,"state":"MERGED"}`,
		"feat/c": `{"number":9,"state":"CLOSED"}`,
	}})
	m := byPane(r.reports)
	for pane, want := range map[string]string{"w1:p1": "#42", "w2:p1": "#7 merged", "w3:p1": "#9 closed"} {
		if got := m[pane].set["ar_git"]; got != want {
			t.Errorf("%s ar_git = %q, want %q", pane, got, want)
		}
	}
}

func TestOneGhLookupPerBranchPerStateChange(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	w := &world{
		Agents: []map[string]any{
			agent("w1:p1", "w1", map[string]any{"cwd": a, "agent_status": "working"}),
			agent("w1:p2", "w1", map[string]any{"cwd": a, "agent_status": "working"}), // same branch
		},
		Workspaces: []map[string]any{ws("w1", "a")},
	}
	open := runOpts{prs: map[string]string{"feat/a": `{"number":1,"state":"OPEN"}`}}
	r1 := sb.run(w, open)
	if len(r1.gh) != 1 {
		t.Fatalf("shared branch lookups = %d, want 1", len(r1.gh))
	}
	apply(w, r1.reports)
	if r2 := sb.run(w, open); len(r2.gh) != 0 {
		t.Errorf("no state change: lookups = %d, want 0", len(r2.gh))
	}
	w.Agents[0]["agent_status"] = "done"
	r3 := sb.run(w, runOpts{prs: map[string]string{"feat/a": `{"number":1,"state":"MERGED"}`}})
	if len(r3.gh) != 1 {
		t.Errorf("state change: lookups = %d, want 1", len(r3.gh))
	}
	if got := byPane(r3.reports)["w1:p1"].set["ar_git"]; got != "#1 merged" {
		t.Errorf("ar_git = %q", got)
	}
}

func TestGhMissing(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a})}, Workspaces: []map[string]any{ws("w1", "a")}},
		runOpts{gh: filepath.Join(sb.dir, "no-such-gh")})
	if got := byPane(r.reports)["w1:p1"].set["ar_git"]; got != "feat/a" {
		t.Errorf("ar_git = %q (%s)", got, r.stderr)
	}
	if r.lockOn {
		t.Error("lock left behind")
	}
	for _, e := range r.state.PRs {
		if e.PR != nil {
			t.Errorf("cached pr = %v, want null", e.PR)
		}
	}
	if len(r.state.PRs) != 1 {
		t.Errorf("prs cached = %d, want 1", len(r.state.PRs))
	}
}

func TestSlowGhIsCutOff(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a})}, Workspaces: []map[string]any{ws("w1", "a")}},
		runOpts{prs: map[string]string{"feat/a": `{"number":1,"state":"OPEN"}`}, ghSleep: 8})
	if r.elapsed > 7*time.Second {
		t.Errorf("sweep took %v", r.elapsed)
	}
	if got := byPane(r.reports)["w1:p1"].set["ar_git"]; got != "feat/a" {
		t.Errorf("ar_git = %q", got)
	}
}

func TestGhCapPerSweep(t *testing.T) {
	sb := newSandbox(t)
	w := &world{}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("w%d", i)
		w.Agents = append(w.Agents, agent(id+":p1", id, map[string]any{"cwd": repo(t, sb.dir, fmt.Sprintf("r%d", i), fmt.Sprintf("feat/%d", i), "main")}))
		w.Workspaces = append(w.Workspaces, ws(id, id))
	}
	r1 := sb.run(w, runOpts{})
	if len(r1.gh) != 4 {
		t.Fatalf("first sweep lookups = %d, want 4", len(r1.gh))
	}
	apply(w, r1.reports)
	if r2 := sb.run(w, runOpts{}); len(r2.gh) != 2 {
		t.Errorf("second sweep lookups = %d, want 2", len(r2.gh))
	}
}

func TestGoneStateDropped(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	r1 := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a}), agent("w2:p1", "w2", nil)}, Workspaces: []map[string]any{ws("w1", "a"), ws("w2", "b")}}, runOpts{})
	if len(r1.state.Stamps) != 2 || len(r1.state.PRs) != 1 {
		t.Fatalf("state = %+v", r1.state)
	}
	r2 := sb.run(&world{Agents: []map[string]any{agent("w2:p1", "w2", nil)}, Workspaces: []map[string]any{ws("w2", "b")}}, runOpts{})
	if _, ok := r2.state.Stamps["term_w2:p1"]; !ok || len(r2.state.Stamps) != 1 {
		t.Errorf("stamps = %v", r2.state.Stamps)
	}
	if len(r2.state.PRs) != 0 {
		t.Errorf("prs = %v", r2.state.PRs)
	}
}

func TestFreshLockHeld(t *testing.T) {
	orig := retryWindow
	retryWindow = 200 * time.Millisecond
	t.Cleanup(func() { retryWindow = orig })
	sb := newSandbox(t)
	os.WriteFile(filepath.Join(sb.stateDir, ".lock"), []byte("99999"), 0o644)
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{})
	if len(r.reports) != 0 || !r.lockOn {
		t.Errorf("reports = %v, lock = %v", r.reports, r.lockOn)
	}
}

func TestStaleLockTakenOver(t *testing.T) {
	sb := newSandbox(t)
	lock := filepath.Join(sb.stateDir, ".lock")
	os.WriteFile(lock, []byte("99999"), 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(lock, old, old)
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{})
	if len(r.reports) != 1 || r.lockOn {
		t.Errorf("reports = %v, lock = %v (%s)", r.reports, r.lockOn, r.stderr)
	}
}

func TestStaleLockRefreshedMidTakeover(t *testing.T) {
	origWindow := retryWindow
	retryWindow = 200 * time.Millisecond
	t.Cleanup(func() { retryWindow = origWindow; beforeTakeover = func() {} })
	sb := newSandbox(t)
	lock := filepath.Join(sb.stateDir, ".lock")
	os.WriteFile(lock, []byte("99999"), 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(lock, old, old)
	beforeTakeover = func() { // the stalled owner wakes and refreshes its lock
		now := time.Now()
		os.Chtimes(lock, now, now)
	}
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{})
	if len(r.reports) != 0 {
		t.Errorf("reports = %v (%s)", r.reports, r.stderr)
	}
	if b, err := os.ReadFile(lock); err != nil || string(b) != "99999" {
		t.Errorf("owner's lock not put back: %q, %v", b, err)
	}
	if left, _ := filepath.Glob(filepath.Join(sb.stateDir, ".lock.stale-*")); len(left) != 0 {
		t.Errorf("takeover tomb left behind: %v", left)
	}
}

// A sweep that crashed holding the lock, followed by the clock stepping back,
// leaves a lock touched in the future. It is taken over, not waited out.
func TestFutureLockTakenOver(t *testing.T) {
	orig := retryWindow
	retryWindow = 200 * time.Millisecond
	t.Cleanup(func() { retryWindow = orig })
	sb := newSandbox(t)
	lock := filepath.Join(sb.stateDir, ".lock")
	os.WriteFile(lock, []byte("99999"), 0o644)
	future := time.Now().Add(time.Hour)
	os.Chtimes(lock, future, future)
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{})
	if len(r.reports) != 1 || r.lockOn {
		t.Errorf("reports = %v, lock = %v (%s)", r.reports, r.lockOn, r.stderr)
	}
}

// A live lock whose mtime is only a little ahead (clock skew on a network
// filesystem) is not taken over.
func TestSlightlyFutureLockKept(t *testing.T) {
	orig := retryWindow
	retryWindow = 200 * time.Millisecond
	t.Cleanup(func() { retryWindow = orig })
	sb := newSandbox(t)
	lock := filepath.Join(sb.stateDir, ".lock")
	os.WriteFile(lock, []byte("99999"), 0o644)
	future := time.Now().Add(5 * time.Second)
	os.Chtimes(lock, future, future)
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{})
	if len(r.reports) != 0 || !r.lockOn {
		t.Errorf("reports = %v, lock = %v (%s)", r.reports, r.lockOn, r.stderr)
	}
}

func TestStateFilesOwnerOnly(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a})}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{})
	for _, name := range []string{"state.json", ".last-sweep"} {
		fi, err := os.Stat(filepath.Join(sb.stateDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s mode = %v, want 0600", name, perm)
		}
	}
}

// After the clock steps back, the last sweep's start lies in the future; it
// must not count as covering new events.
func TestFutureLastSweepIgnored(t *testing.T) {
	sb := newSandbox(t)
	stamp := filepath.Join(sb.stateDir, ".last-sweep")
	os.WriteFile(stamp, nil, 0o600)
	future := time.Now().Add(time.Hour)
	os.Chtimes(stamp, future, future)
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{})
	if len(r.reports) != 1 {
		t.Errorf("reports = %v, want a sweep (%s)", r.reports, r.stderr)
	}
	if fi, err := os.Stat(stamp); err != nil {
		t.Errorf("stamp: %v", err)
	} else if fi.ModTime().After(time.Now()) {
		t.Errorf("stamp not reset to the sweep's start: %v", fi.ModTime())
	}
}

// A state dir that can't hold a lock fails the event at once, naming the
// dir, rather than waiting out the lock retry window.
func TestUnusableStateDirFailsFast(t *testing.T) {
	sb := newSandbox(t)
	file := filepath.Join(sb.dir, "file")
	os.WriteFile(file, nil, 0o600)
	dirs := map[string]string{"not a directory": filepath.Join(file, "state")}
	if os.Geteuid() != 0 { // root writes through a read-only mode
		ro := filepath.Join(sb.dir, "ro")
		os.MkdirAll(ro, 0o500)
		t.Cleanup(func() { os.Chmod(ro, 0o700) })
		dirs["read-only"] = ro
	}
	for name, dir := range dirs {
		r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{stateDir: dir})
		if r.elapsed > time.Second {
			t.Errorf("%s: took %v", name, r.elapsed)
		}
		if len(r.reports) != 0 || strings.Count(r.stderr, "\n") != 1 || !strings.Contains(r.stderr, dir) || strings.Contains(r.stderr, "busy") {
			t.Errorf("%s: reports = %v, stderr = %q", name, r.reports, r.stderr)
		}
	}
}

// Two herdr sessions share the plugin's state dir. Each sweep drops state
// for agents it can't see, so sessions must not share state files.
func TestSessionsKeepSeparateState(t *testing.T) {
	a, b := newSandbox(t), newSandbox(t)
	repoA := repo(t, a.dir, "a", "feat/a", "main")
	wa := &world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": repoA, "agent_status": "working"})}, Workspaces: []map[string]any{ws("w1", "a")}}
	apply(wa, a.run(wa, runOpts{}).reports)
	wa.Agents[0]["agent_status"] = "done"
	apply(wa, a.run(wa, runOpts{}).reports) // "done hh:mm", PR looked up

	wb := &world{Agents: []map[string]any{agent("w9:p1", "w9", nil)}, Workspaces: []map[string]any{ws("w9", "b")}}
	b.run(wb, runOpts{stateDir: a.base}) // session b, same plugin state dir

	r := a.run(wa, runOpts{})
	if len(r.reports) != 0 || len(r.gh) != 0 {
		t.Errorf("session a lost its state: reports = %v, gh = %v", r.reports, r.gh)
	}
	if st := r.state.Stamps["term_w1:p1"]; st == nil || !st.Seen {
		t.Errorf("session a stamp = %+v", st)
	}
	for _, name := range []string{a.stateDir, filepath.Join(a.base, sessionDir(b.sock))} {
		if fi, err := os.Stat(name); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", name, fi, err)
		}
	}
}

// A sweep removes other sessions' state dirs that have been idle for 30
// days, at most once a day. Anything it doesn't recognise as one is kept.
func TestIdleSessionsPruned(t *testing.T) {
	sb := newSandbox(t)
	old := time.Now().Add(-31 * 24 * time.Hour)
	// mkSession makes a state dir under base whose files, and the dir itself,
	// were last touched at mtime.
	mkSession := func(base, name string, mtime time.Time, files ...string) string {
		dir := filepath.Join(base, name)
		os.MkdirAll(dir, 0o700)
		for _, f := range files {
			os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o600)
			os.Chtimes(filepath.Join(dir, f), mtime, mtime)
		}
		os.Chtimes(dir, mtime, mtime)
		return dir
	}
	idle := mkSession(sb.base, "session-00000000000000aa", old, "state.json", ".last-sweep", ".lock")
	recent := mkSession(sb.base, "session-00000000000000bb", old, "state.json")
	os.WriteFile(filepath.Join(recent, ".last-sweep"), nil, 0o600) // touched now
	os.Chtimes(recent, old, old)
	var kept []string
	for _, name := range []string{"session-old", "session-00000000000000FF", "session-00000000000000aaa", "other"} {
		kept = append(kept, mkSession(sb.base, name, old, "state.json"))
	}
	nested := mkSession(sb.base, "session-00000000000000cc", old, "state.json")
	mkSession(nested, "sub", old, "keep")
	os.Chtimes(nested, old, old)
	target := mkSession(sb.dir, "elsewhere", old, "state.json")
	link := filepath.Join(sb.base, "session-00000000000000dd")
	os.Symlink(target, link)
	kept = append(kept, recent, nested, target, link)
	kept = append(kept, sb.stateDir)

	w := &world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}
	sb.run(w, runOpts{dry: true})
	if _, err := os.Lstat(idle); err != nil {
		t.Errorf("dry run pruned: %v", err)
	}
	r := sb.run(w, runOpts{})
	if _, err := os.Lstat(idle); err == nil {
		t.Errorf("idle session dir kept")
	}
	for _, p := range kept {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
	if r.state.Pruned == 0 {
		t.Errorf("prune time not recorded")
	}

	idle2 := mkSession(sb.base, "session-00000000000000ee", old, "state.json")
	sb.run(w, runOpts{})
	if _, err := os.Lstat(idle2); err != nil {
		t.Errorf("pruned again within a day: %v", err)
	}
}

func TestRefusesOutsideHerdr(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{noStateDir: true})
	if len(r.reports) != 0 || !strings.Contains(r.stderr, "HERDR_PLUGIN_STATE_DIR is not set") {
		t.Errorf("reports = %v, stderr = %q", r.reports, r.stderr)
	}
}

func TestDryRun(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", nil)}, Workspaces: []map[string]any{ws("w1", "a")}}, runOpts{dry: true})
	if len(r.reports) != 0 {
		t.Errorf("reports = %v", r.reports)
	}
	if !regexp.MustCompile(`(?m)^w1:p1 ar_name="a" ar_git=- ar_state="idle" ar_title=- ar_wname=- ar_wgit=- ar_wstate=-$`).MatchString(r.stdout) {
		t.Errorf("stdout = %q", r.stdout)
	}
}

func TestBadSnapshotShape(t *testing.T) {
	sb := newSandbox(t)
	r := sb.run(&world{noAgents: true, Workspaces: []map[string]any{}}, runOpts{})
	if len(r.reports) != 0 || !strings.Contains(r.stderr, "unexpected herdr snapshot shape") {
		t.Errorf("reports = %v, stderr = %q", r.reports, r.stderr)
	}
}

// ---- git file reading ------------------------------------------------------------

// spawnCount swaps in a counting spawnGit for one test.
func spawnCount(t *testing.T) *int {
	n := 0
	orig := spawnGit
	spawnGit = func(cwd string, args ...string) (string, bool) {
		n++
		return orig(cwd, args...)
	}
	t.Cleanup(func() { spawnGit = orig })
	return &n
}

func TestGitWorktree(t *testing.T) {
	root := t.TempDir()
	main := repo(t, root, "main", "main", "main")
	git(t, main, "commit", "-q", "--allow-empty", "-m", "init")
	wt := filepath.Join(root, "wt")
	git(t, main, "worktree", "add", "-q", "-b", "feat/wt", wt)
	sub := filepath.Join(wt, "sub", "dir")
	os.MkdirAll(sub, 0o755)
	n := spawnCount(t)
	info := readGit(sub)
	if info == nil || info.Branch != "feat/wt" || !info.OffDefault || info.Dir != sub {
		t.Fatalf("info = %+v", info)
	}
	if *n != 0 {
		t.Errorf("spawned git %d times, want 0", *n)
	}
	// A worktree on the default branch is on-default (origin/HEAD via commondir).
	git(t, wt, "switch", "-q", "--detach")
	git(t, main, "switch", "-q", "--detach")
	git(t, wt, "switch", "-q", "main")
	if info := readGit(wt); info == nil || info.OffDefault {
		t.Errorf("worktree on main: %+v", info)
	}
}

func TestGitDetachedHead(t *testing.T) {
	dir := repo(t, t.TempDir(), "r", "main", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, dir, "switch", "-q", "--detach")
	n := spawnCount(t)
	if info := readGit(dir); info != nil {
		t.Errorf("detached: %+v", info)
	}
	if *n != 0 {
		t.Errorf("spawned git %d times, want 0", *n)
	}
}

func TestGitMissingOriginHead(t *testing.T) {
	root := t.TempDir()
	n := spawnCount(t)
	for branch, off := range map[string]bool{"main": false, "master": false, "feat/z": true} {
		info := readGit(repo(t, root, strings.ReplaceAll(branch, "/", "-"), branch, ""))
		if info == nil || info.OffDefault != off {
			t.Errorf("%s: %+v", branch, info)
		}
	}
	if *n != 0 {
		t.Errorf("spawned git %d times, want 0", *n)
	}
}

// Files and `git symbolic-ref` must agree across layouts.
func TestGitMatchesSpawn(t *testing.T) {
	root := t.TempDir()
	dirs := []string{
		repo(t, root, "a", "feat/a", "main"),
		repo(t, root, "b", "main", "trunk"),
		repo(t, root, "c", "master", ""),
	}
	for _, d := range dirs {
		got, want := readGit(d), spawnGitInfo(d)
		if got != nil && want != nil {
			got.Repo, want.Repo = "", "" // the git dir vs the fallback's cwd, by design
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: files %+v, spawn %+v", d, got, want)
		}
	}
}

func TestGitFallbackOnUnknownLayout(t *testing.T) {
	root := t.TempDir()
	// reftable stores refs outside the loose-file layout.
	dir := filepath.Join(root, "rt")
	os.MkdirAll(dir, 0o755)
	if out, err := exec.Command("git", "-C", dir, "init", "-q", "--ref-format=reftable", "-b", "feat/rt").CombinedOutput(); err != nil {
		t.Skipf("git has no reftable support: %s", out)
	}
	n := spawnCount(t)
	info := readGit(dir)
	if info == nil || info.Branch != "feat/rt" || !info.OffDefault {
		t.Errorf("reftable: %+v", info)
	}
	if *n == 0 {
		t.Error("expected the git fallback")
	}
}

func TestGitFallbackOnUnexpectedHead(t *testing.T) {
	dir := repo(t, t.TempDir(), "r", "main", "")
	os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("something else\n"), 0o644)
	n := spawnCount(t)
	readGit(dir)
	if *n == 0 {
		t.Error("expected the git fallback")
	}
}

// git plumbing accepts a branch named like a gh flag; it must reach gh as the
// branch argument, not as an option.
func TestFlagShapedBranchIsNotAnOption(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "main", "main")
	git(t, a, "symbolic-ref", "HEAD", "refs/heads/--repo=other/repo")
	r := sb.run(&world{Agents: []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a})}, Workspaces: []map[string]any{ws("w1", "a")}},
		runOpts{prs: map[string]string{"--repo=other/repo": `{"number":3,"state":"OPEN"}`}})
	if len(r.gh) != 1 || r.gh[0] != "pr list --head=--repo=other/repo --state all --json number,state,isCrossRepository --limit 20" {
		t.Errorf("gh calls = %q", r.gh)
	}
	if got := byPane(r.reports)["w1:p1"].set["ar_git"]; got != "#3" {
		t.Errorf("ar_git = %q", got)
	}
}

// A branch named like a PR number is matched as a head branch, never read as
// the PR with that number.
func TestNumericBranchIsNotAPrNumber(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "42", "main")
	b := repo(t, sb.dir, "b", "#7", "main")
	r := sb.run(&world{
		Agents:     []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a}), agent("w2:p1", "w2", map[string]any{"cwd": b})},
		Workspaces: []map[string]any{ws("w1", "a"), ws("w2", "b")},
	}, runOpts{})
	m := byPane(r.reports)
	if m["w1:p1"].set["ar_git"] != "42" || m["w2:p1"].set["ar_git"] != "#7" {
		t.Errorf("ar_git = %q / %q, want the branch names", m["w1:p1"].set["ar_git"], m["w2:p1"].set["ar_git"])
	}
	if len(r.gh) != 2 {
		t.Errorf("gh calls = %q, want one per branch", r.gh)
	}
	for _, call := range r.gh {
		if !strings.Contains(call, "--head=42 ") && !strings.Contains(call, "--head=#7 ") {
			t.Errorf("gh call not by head branch: %q", call)
		}
	}
}

// Of several PRs from one branch, an open one wins, then the highest number.
// PRs from forks with a branch of the same name don't count.
func TestPrChoiceAmongSeveral(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	b := repo(t, sb.dir, "b", "feat/b", "main")
	r := sb.run(&world{
		Agents:     []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a}), agent("w2:p1", "w2", map[string]any{"cwd": b})},
		Workspaces: []map[string]any{ws("w1", "a"), ws("w2", "b")},
	}, runOpts{prs: map[string]string{
		"feat/a": `[{"number":9,"state":"CLOSED"},{"number":5,"state":"OPEN"},{"number":3,"state":"MERGED"},{"number":12,"state":"OPEN","isCrossRepository":true}]`,
		"feat/b": `[{"number":3,"state":"MERGED"},{"number":8,"state":"CLOSED"},{"number":11,"state":"OPEN","isCrossRepository":true}]`,
	}})
	m := byPane(r.reports)
	if got := m["w1:p1"].set["ar_git"]; got != "#5" {
		t.Errorf("open PR: ar_git = %q, want #5", got)
	}
	if got := m["w2:p1"].set["ar_git"]; got != "#8 closed" {
		t.Errorf("newest PR: ar_git = %q, want #8 closed", got)
	}
}

// A FIFO where a git file should be would block a plain read forever.
func TestGitFilesMustBeRegular(t *testing.T) {
	root := t.TempDir()
	fifo := func(p string) {
		if err := exec.Command("mkfifo", p).Run(); err != nil {
			t.Skipf("mkfifo: %v", err)
		}
	}
	dotGit := filepath.Join(root, "dotgit")
	os.MkdirAll(dotGit, 0o755)
	fifo(filepath.Join(dotGit, ".git"))
	head := repo(t, root, "head", "feat/h", "main")
	os.Remove(filepath.Join(head, ".git", "HEAD"))
	fifo(filepath.Join(head, ".git", "HEAD"))
	n := spawnCount(t)
	got := make(chan [2]*gitInfo, 1)
	go func() { got <- [2]*gitInfo{readGit(dotGit), readGit(head)} }()
	select {
	case infos := <-got:
		if infos[0] != nil || infos[1] != nil {
			t.Errorf("fifo read as a repo: %+v %+v", infos[0], infos[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("readGit blocked on a fifo")
	}
	if *n != 0 {
		t.Errorf("spawned git %d times, want 0 (git blocks on a fifo too)", *n)
	}
}

func TestGhCapServesNeverLookedUpFirst(t *testing.T) {
	sb := newSandbox(t)
	w := &world{}
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("w%d", i)
		w.Agents = append(w.Agents, agent(id+":p1", id, map[string]any{"cwd": repo(t, sb.dir, fmt.Sprintf("r%d", i), fmt.Sprintf("feat/%d", i), "main"), "agent_status": "working"}))
		w.Workspaces = append(w.Workspaces, ws(id, id))
	}
	apply(w, sb.run(w, runOpts{}).reports) // looks up feat/0..3
	for i := 0; i < 4; i++ {
		w.Agents[i]["agent_status"] = "done" // busy agents early in the list are due again
	}
	r := sb.run(w, runOpts{})
	got := strings.Join(r.gh, "\n")
	for _, b := range []string{"feat/4", "feat/5"} {
		if !strings.Contains(got, b) {
			t.Errorf("%s starved; lookups:\n%s", b, got)
		}
	}
}

func TestPrLookupSharedAcrossSubdirs(t *testing.T) {
	sb := newSandbox(t)
	a := repo(t, sb.dir, "a", "feat/a", "main")
	sub := filepath.Join(a, "web")
	os.MkdirAll(sub, 0o755)
	r := sb.run(&world{
		Agents:     []map[string]any{agent("w1:p1", "w1", map[string]any{"cwd": a}), agent("w1:p2", "w1", map[string]any{"cwd": sub})},
		Workspaces: []map[string]any{ws("w1", "a")},
	}, runOpts{prs: map[string]string{"feat/a": `{"number":5,"state":"OPEN"}`}})
	if len(r.gh) != 1 {
		t.Errorf("lookups = %d, want 1", len(r.gh))
	}
	m := byPane(r.reports)
	if m["w1:p1"].set["ar_git"] != "#5" || m["w1:p2"].set["ar_git"] != "#5" {
		t.Errorf("ar_git = %q / %q", m["w1:p1"].set["ar_git"], m["w1:p2"].set["ar_git"])
	}
}

// Each way a worker's pane can leave needs a hook, or its lead's roll-up
// waits for the lead's next state change. Closing a tab fires tab.closed
// alone and closing a workspace fires workspace.closed alone, never
// pane.closed for the panes inside. A worker's lead is linked by pane ID,
// so it can sit in another workspace.
func TestManifestHooksPaneRemoval(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "herdr-plugin.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var hooked []string
	for _, m := range regexp.MustCompile(`(?m)^\s*on\s*=\s*["']([^"']+)["']`).FindAllStringSubmatch(string(b), -1) {
		hooked = append(hooked, m[1])
	}
	for _, ev := range []string{"pane.closed", "pane.exited", "tab.closed", "workspace.closed"} {
		if !slices.Contains(hooked, ev) {
			t.Errorf("herdr-plugin.toml does not hook %s (hooks %v)", ev, hooked)
		}
	}
}
