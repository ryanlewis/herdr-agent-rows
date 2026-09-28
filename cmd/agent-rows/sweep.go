package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Tokens this plugin owns, in report order. The sidebar config renders them;
// `role` and `lead` are optional read-only inputs, set by whatever groups
// agents under a lead.
// Workers get their own ar_w* tokens so the config can mute their whole row
// (style rules only see a token's own value).
var tokenNames = []string{"ar_name", "ar_git", "ar_state", "ar_title", "ar_wname", "ar_wgit", "ar_wstate"}

// workerIndent sets a worker's name in from the state icon. herdr trims
// leading whitespace from token values; U+2800 (braille blank) is not
// whitespace, so it survives and renders as blank cells.
const workerIndent = "\u2800\u2800"

// ---- state ------------------------------------------------------------------
// state.json: {
//   stamps: { <terminal id>: { cls, since, seen } } — last state class and when
//           it began (ms); `seen` is false for a pane first met mid-state (we
//           don't know when that state began, so no time is shown).
//   prs:    { <git dir>|<branch>: { at, pr } }      — last gh answer (ms).
// }

type stamp struct {
	Cls   string `json:"cls"`
	Since int64  `json:"since"`
	Seen  bool   `json:"seen"`
}

type prEntry struct {
	At int64 `json:"at"`
	PR *pr   `json:"pr"`
}

type state struct {
	Stamps map[string]*stamp   `json:"stamps"`
	PRs    map[string]*prEntry `json:"prs"`
}

func readState(c *config) *state {
	s := &state{}
	if b, err := os.ReadFile(filepath.Join(c.stateDir, "state.json")); err == nil {
		_ = json.Unmarshal(b, s)
	}
	if s.Stamps == nil {
		s.Stamps = map[string]*stamp{}
	}
	if s.PRs == nil {
		s.PRs = map[string]*prEntry{}
	}
	return s
}

func writeState(c *config, s *state) {
	if c.dry {
		return
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err == nil {
		// Atomic replace so a concurrent reader never sees a torn file. Owner
		// only: it holds working directories and branch names.
		tmp := filepath.Join(c.stateDir, "state.json.tmp-"+strconv.Itoa(os.Getpid()))
		if err = os.WriteFile(tmp, append(b, '\n'), 0o600); err == nil {
			err = os.Rename(tmp, filepath.Join(c.stateDir, "state.json"))
		}
	}
	if err != nil {
		c.warn("state write failed: %v", err)
	}
}

// ---- row model --------------------------------------------------------------

// norm applies herdr's own normalization (trim, drop control chars, cap at 80
// chars, trim), so an unchanged value compares equal to what herdr hands back
// and is never re-reported. "" means no value.
func norm(v string) string {
	r := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(v)))
	if len(r) > maxValueRunes {
		r = r[:maxValueRunes]
	}
	return strings.TrimSpace(string(r))
}

// State class drives the stamp; done and idle are one class (done is just
// idle-not-yet-seen), so looking at a finished pane keeps its finish time.
func stateClass(status string) string {
	switch status {
	case "done", "idle":
		return "stopped"
	case "working", "blocked":
		return status
	}
	return "unknown"
}

// `done 14:02`, `blocked 14:05`; nothing while working (the icon says it).
func stateText(status string, st *stamp) string {
	if status != "done" && status != "idle" && status != "blocked" {
		return ""
	}
	if st != nil && st.Seen {
		return status + " " + time.UnixMilli(st.Since).Format("15:04")
	}
	return status
}

func rollup(workers []agentInfo) string {
	if len(workers) == 0 {
		return "lead"
	}
	blocked := 0
	for _, w := range workers {
		if w.AgentStatus == "blocked" {
			blocked++
		}
	}
	s := fmt.Sprintf("lead · %d worker", len(workers))
	if len(workers) != 1 {
		s += "s"
	}
	if blocked > 0 {
		s += fmt.Sprintf(", %d blocked", blocked)
	}
	return s
}

func sameText(a, b string) bool {
	return a != "" && b != "" && strings.ToLower(strings.TrimSpace(a)) == strings.ToLower(strings.TrimSpace(b))
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if n := norm(v); n != "" {
			return n
		}
	}
	return ""
}

type rowCtx struct {
	wsLabel   map[string]string
	paneLabel map[string]string
	stamps    map[string]*stamp
	git       map[string]string
}

// computeRows returns the tokens each agent should carry, by pane id.
func computeRows(agents []agentInfo, ctx rowCtx) map[string]map[string]string {
	leads := map[string]bool{}
	for _, a := range agents {
		if a.Tokens["role"] == "lead" {
			leads[a.PaneID] = true
		}
	}
	isWorker := func(a agentInfo) bool {
		return a.Tokens["role"] == "worker" && leads[a.Tokens["lead"]] && a.Tokens["lead"] != a.PaneID
	}
	workersOf := map[string][]agentInfo{}
	for _, a := range agents {
		if isWorker(a) {
			workersOf[a.Tokens["lead"]] = append(workersOf[a.Tokens["lead"]], a)
		}
	}

	rows := map[string]map[string]string{}
	for _, a := range agents {
		ws := ctx.wsLabel[a.WorkspaceID]
		git := ctx.git[a.PaneID]
		st := ctx.stamps[a.PaneID]
		var want map[string]string
		switch {
		case a.Tokens["role"] == "lead":
			want = map[string]string{"ar_name": ws, "ar_git": git, "ar_state": rollup(workersOf[a.PaneID])}
		case isWorker(a):
			// Worker name: herdr agent name (set by `agent start --name`), else
			// the pane label, else the title. Pane labels can be rewritten by
			// other plugins; the agent name only changes on an explicit rename.
			name := firstNonEmpty(a.Name, ctx.paneLabel[a.PaneID], a.TerminalTitleStripped, a.PaneID)
			want = map[string]string{"ar_wname": workerIndent + name, "ar_wgit": git, "ar_wstate": stateText(a.AgentStatus, st)}
		default:
			title := a.TerminalTitleStripped
			if sameText(title, ws) {
				title = ""
			}
			want = map[string]string{"ar_name": ws, "ar_git": git, "ar_state": stateText(a.AgentStatus, st), "ar_title": title}
		}
		for k, v := range want {
			want[k] = norm(v)
		}
		rows[a.PaneID] = want
	}
	return rows
}

// ---- sweep ------------------------------------------------------------------

func sweep(c *config) error {
	snap, err := sessionSnapshot(c.socket)
	if err != nil {
		return err
	}
	if snap.Agents == nil || snap.Workspaces == nil {
		return errors.New("unexpected herdr snapshot shape; no-op")
	}
	var live []agentInfo
	for _, a := range *snap.Agents {
		if a.PaneID != "" {
			live = append(live, a)
		}
	}
	touchLock(c)

	st := readState(c)
	dirty := false
	now := time.Now().UnixMilli()

	// Stamps: keyed by terminal id so a pane id reused by a new terminal
	// doesn't inherit an old time. Drop stamps for terminals that are gone.
	keyOf := func(a agentInfo) string {
		if a.TerminalID != "" {
			return a.TerminalID
		}
		return a.PaneID
	}
	liveKeys := map[string]bool{}
	for _, a := range live {
		liveKeys[keyOf(a)] = true
	}
	for k := range st.Stamps {
		if !liveKeys[k] {
			delete(st.Stamps, k)
			dirty = true
		}
	}
	stamps := map[string]*stamp{}
	for _, a := range live {
		k := keyOf(a)
		cls := stateClass(a.AgentStatus)
		prev := st.Stamps[k]
		if prev == nil || prev.Cls != cls {
			// First sight: we don't know when this state began, so no time.
			st.Stamps[k] = &stamp{Cls: cls, Since: now, Seen: prev != nil}
			dirty = true
		}
		stamps[a.PaneID] = st.Stamps[k]
	}

	// Git: once per distinct cwd per sweep.
	byCwd := map[string]*gitInfo{}
	infoOf := map[string]*gitInfo{}
	for _, a := range live {
		cwd := a.ForegroundCwd
		if cwd == "" {
			cwd = a.Cwd
		}
		if cwd == "" {
			continue
		}
		info, seen := byCwd[cwd]
		if !seen {
			// Each git fallback can take up to its 2 s timeout; keep the
			// lock live across many cwds.
			touchLock(c)
			info = readGit(cwd)
			byCwd[cwd] = info
		}
		if info != nil && info.OffDefault {
			infoOf[a.PaneID] = info
		}
	}

	// PRs: one gh lookup per branch per state change. A branch is due when
	// it has never been looked up, or an agent on it changed state since.
	// Keyed by checkout, not cwd, so agents in different subdirectories of
	// one checkout (or one agent that cds around) share a lookup.
	prKey := func(i *gitInfo) string { return i.Repo + "|" + i.Branch }
	var due []*gitInfo
	dueSet := map[string]bool{}
	for _, a := range live {
		info := infoOf[a.PaneID]
		if info == nil {
			continue
		}
		k := prKey(info)
		e := st.PRs[k]
		if !dueSet[k] && (e == nil || e.At < stamps[a.PaneID].Since) {
			dueSet[k] = true
			due = append(due, info)
		}
	}
	if len(due) > ghMaxPerSweep {
		// Never-looked-up first, then the stalest, so busy agents early in
		// the list can't starve the rest of the cap.
		at := func(i *gitInfo) int64 {
			if e := st.PRs[prKey(i)]; e != nil {
				return e.At
			}
			return 0
		}
		sort.SliceStable(due, func(x, y int) bool { return at(due[x]) < at(due[y]) })
		due = due[:ghMaxPerSweep]
	}
	results := make([]*pr, len(due))
	var wg sync.WaitGroup
	for i, info := range due {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = lookupPr(c.gh, info.Dir, info.Branch)
		}()
	}
	wg.Wait()
	for i, info := range due {
		st.PRs[prKey(info)] = &prEntry{At: time.Now().UnixMilli(), PR: results[i]}
		dirty = true
	}
	// Forget PR answers for branches no live agent is on.
	liveBranches := map[string]bool{}
	for _, info := range infoOf {
		liveBranches[prKey(info)] = true
	}
	for k := range st.PRs {
		if !liveBranches[k] {
			delete(st.PRs, k)
			dirty = true
		}
	}
	touchLock(c)

	gitLabel := map[string]string{}
	for paneID, info := range infoOf {
		label := ""
		if e := st.PRs[prKey(info)]; e != nil {
			label = prLabel(e.PR)
		}
		if label == "" {
			label = info.Branch
		}
		gitLabel[paneID] = label
	}

	wsLabel := map[string]string{}
	for _, w := range *snap.Workspaces {
		wsLabel[w.WorkspaceID] = w.Label
	}
	paneLabel := map[string]string{}
	if snap.Panes != nil {
		for _, p := range *snap.Panes {
			paneLabel[p.PaneID] = p.Label
		}
	}
	rows := computeRows(live, rowCtx{wsLabel: wsLabel, paneLabel: paneLabel, stamps: stamps, git: gitLabel})

	if dirty {
		writeState(c, st)
	}

	for _, a := range live {
		want := rows[a.PaneID]
		patch := map[string]*string{}
		for _, tok := range tokenNames {
			v := want[tok]
			have, has := a.Tokens[tok]
			if v == "" {
				if has {
					patch[tok] = nil
				}
			} else if !has || have != v {
				patch[tok] = &v
			}
		}
		if c.dry {
			var line bytes.Buffer
			line.WriteString(a.PaneID)
			for _, tok := range tokenNames {
				if want[tok] == "" {
					fmt.Fprintf(&line, " %s=-", tok)
				} else {
					fmt.Fprintf(&line, " %s=%s", tok, jsonString(want[tok]))
				}
			}
			if len(patch) == 0 {
				line.WriteString(" (unchanged)")
			}
			fmt.Fprintln(c.stdout, line.String())
			continue
		}
		if len(patch) == 0 {
			continue // already current: no herdr call
		}
		touchLock(c)
		if err := reportTokens(c.socket, a.PaneID, patch); err != nil {
			c.warn("report for %s failed: %v", a.PaneID, err)
		}
	}
	return nil
}

func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}
