package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type gitInfo struct {
	Dir        string // the cwd itself; gh resolves the repo from any subdirectory
	Repo       string // the checkout's git dir (the cwd for the git fallback): PR cache key
	Branch     string
	OffDefault bool
}

// readGit returns the checkout's branch and whether it is off the default
// branch, or nil outside a repo or on a detached HEAD. The default branch is
// origin/HEAD when the clone has one, else main/master.
//
// It reads the git files directly rather than spawning git twice per cwd on
// every sweep: walk up to .git, follow a `gitdir:` file for worktrees, read
// HEAD, and read <commondir>/refs/remotes/origin/HEAD. Symbolic refs are
// always loose files (packed-refs can't hold them), so this is what
// `git symbolic-ref` reads too. Any layout it doesn't recognise (reftable,
// an unexpected HEAD) falls back to spawning git.
func readGit(cwd string) *gitInfo {
	gitDir, ok := findGitDir(cwd)
	if !ok {
		return nil // not a repo
	}
	common := gitDir
	if b, err := readSmall(filepath.Join(gitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitDir, c)
		}
		common = c
	}
	if isDir(filepath.Join(common, "reftable")) {
		return spawnGitInfo(cwd)
	}
	head, err := readSmall(filepath.Join(gitDir, "HEAD"))
	if errors.Is(err, errNotGitFile) {
		return nil // git would block on it too
	}
	if err != nil {
		return spawnGitInfo(cwd)
	}
	branch, sym := symref(head, "refs/heads/")
	if !sym {
		if isObjectID(strings.TrimSpace(string(head))) {
			return nil // detached
		}
		return spawnGitInfo(cwd)
	}
	def := ""
	if b, err := readSmall(filepath.Join(common, "refs", "remotes", "origin", "HEAD")); err == nil {
		d, ok := symref(b, "refs/remotes/origin/")
		if !ok {
			return spawnGitInfo(cwd)
		}
		def = d
	}
	return makeInfo(cwd, gitDir, branch, def)
}

func makeInfo(cwd, repo, branch, def string) *gitInfo {
	off := branch != "main" && branch != "master"
	if def != "" {
		off = branch != def
	}
	return &gitInfo{Dir: cwd, Repo: repo, Branch: branch, OffDefault: off}
}

// symref parses "ref: <prefix><name>".
func symref(b []byte, prefix string) (string, bool) {
	s := strings.TrimSpace(string(b))
	name, ok := strings.CutPrefix(s, "ref: "+prefix)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// readSmall reads a git metadata file, refusing anything that is not a
// regular file or is larger than any real one. The cwd comes from whatever
// the agent is working in, so a FIFO or device named like a git file must
// not hang or flood the sweep. O_NONBLOCK keeps the open itself from
// blocking on a FIFO; the type is then checked on the open file.
func readSmall(p string) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", p, errNotGitFile)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxGitFile+1))
	if err == nil && len(b) > maxGitFile {
		err = fmt.Errorf("%s: %w", p, errNotGitFile)
	}
	return b, err
}

// errNotGitFile: a non-regular or oversized file where git keeps a small one.
var errNotGitFile = errors.New("not a git metadata file")

// HEAD, commondir and a .git file each hold one ref or path.
const maxGitFile = 4096

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// findGitDir walks up from cwd to the first .git: a directory, or a file
// holding "gitdir: <path>" (worktrees, submodules).
func findGitDir(cwd string) (string, bool) {
	dir := filepath.Clean(cwd)
	for {
		p := filepath.Join(dir, ".git")
		if fi, err := os.Stat(p); err == nil {
			if fi.IsDir() {
				return p, true
			}
			b, err := readSmall(p)
			if err != nil {
				return "", false
			}
			g, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
			if !ok {
				return "", false
			}
			if !filepath.IsAbs(g) {
				g = filepath.Join(dir, g)
			}
			return g, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// spawnGit runs git; a package var so tests can see the fallback taken.
var spawnGit = func(cwd string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	// As for gh: don't wait on children still holding stdout after a kill.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func spawnGitInfo(cwd string) *gitInfo {
	branch, ok := spawnGit(cwd, "symbolic-ref", "-q", "--short", "HEAD")
	if !ok || branch == "" {
		return nil
	}
	def := ""
	if o, ok := spawnGit(cwd, "symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); ok {
		if _, d, found := strings.Cut(o, "/"); found {
			def = d
		}
	}
	return makeInfo(cwd, cwd, branch, def)
}

// ---- gh -------------------------------------------------------------------

type pr struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// lookupPr returns the branch's PR, or nil for no PR, gh missing, gh
// failing or gh too slow. It matches PRs by head branch, so a branch named
// like a PR number ("42", "#42") is not read as one. PRs from forks are
// skipped: a fork's branch of the same name is someone else's work. Of
// several PRs from the branch, an open one wins, then the most recent
// (highest number).
func lookupPr(gh, dir, branch string) *pr {
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	// --head=<branch> as one argument, so a branch named like a flag (git
	// plumbing allows "--repo=x") stays the flag's value.
	cmd := exec.CommandContext(ctx, gh, "pr", "list", "--head="+branch, "--state", "all",
		"--json", "number,state,isCrossRepository", "--limit", strconv.Itoa(ghPrLimit))
	cmd.Dir = dir
	// A killed gh can leave children holding stdout open; don't wait on them.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var prs []struct {
		Number          *int   `json:"number"`
		State           string `json:"state"`
		CrossRepository bool   `json:"isCrossRepository"`
	}
	if json.Unmarshal(out, &prs) != nil {
		return nil
	}
	var best *pr
	for _, p := range prs {
		if p.Number == nil || p.CrossRepository {
			continue
		}
		c := &pr{Number: *p.Number, State: p.State}
		if best == nil || prefer(c, best) {
			best = c
		}
	}
	return best
}

// How many PRs from one branch to consider.
const ghPrLimit = 20

func prefer(a, b *pr) bool {
	aOpen, bOpen := strings.EqualFold(a.State, "OPEN"), strings.EqualFold(b.State, "OPEN")
	if aOpen != bOpen {
		return aOpen
	}
	return a.Number > b.Number
}

func prLabel(p *pr) string {
	if p == nil {
		return ""
	}
	switch strings.ToUpper(p.State) {
	case "MERGED":
		return "#" + strconv.Itoa(p.Number) + " merged"
	case "CLOSED":
		return "#" + strconv.Itoa(p.Number) + " closed"
	}
	return "#" + strconv.Itoa(p.Number)
}
