// Agent Rows — compute per-pane sidebar tokens so herdr's Agent panel shows
// de-duplicated titles, branch/PR and a stamped state time, and optionally
// groups workers under a lead when panes carry role/lead tokens.
//
// Global idempotent reconcile: every invocation sweeps all agents (event
// payload ignored), so a missed event self-heals on the next one.
//
// Fail-safe posture: any parse/shape surprise → skip + one line to stderr.
// A stale row is acceptable; a broken sweep or a hung gh is not.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

const (
	metadataSource = "io.rlew.agent-rows"
	lockStale      = 30 * time.Second
	// gh runs under the sweep lock, so it is bounded twice: per call, and in
	// how many run per sweep (in parallel). Lookups past the cap stay due and
	// run on the next sweep.
	ghTimeout     = 4 * time.Second
	ghMaxPerSweep = 4
	maxValueRunes = 80 // herdr caps token values at 80 chars
)

// A waiting event must outlast a sweep that is running gh (ghTimeout plus
// the 1 s WaitDelay), or it gives up while that sweep is still going. A var
// so tests can shorten it.
var retryWindow = ghTimeout + 4*time.Second

type config struct {
	dry      bool
	socket   string
	stateDir string // plugin state dir, narrowed by run to this session's; "" outside herdr without --dry-run
	gh       string
	stdout   io.Writer
	stderr   io.Writer
}

func (c *config) warn(format string, a ...any) {
	fmt.Fprintf(c.stderr, "agent-rows: "+format+"\n", a...)
}

func main() {
	dry := slices.Contains(os.Args[1:], "--dry-run")
	stateDir := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	// --dry-run from a plain shell reads the real stamp/PR cache so the
	// preview matches event-driven output; real runs require the env var
	// (proof we're running under herdr).
	if stateDir == "" && dry {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			home, _ := os.UserHomeDir()
			base = filepath.Join(home, ".local", "state")
		}
		stateDir = filepath.Join(base, "herdr", "plugins", metadataSource)
	}
	run(&config{dry: dry, socket: socketPath(), stateDir: stateDir, gh: "gh", stdout: os.Stdout, stderr: os.Stderr})
}

// run is the entrypoint minus process setup, so tests can drive it.
func run(c *config) {
	if c.stateDir == "" && !c.dry {
		c.warn("HERDR_PLUGIN_STATE_DIR is not set (not running under herdr?); use --dry-run to preview")
		return
	}
	if c.stateDir != "" {
		c.stateDir = filepath.Join(c.stateDir, sessionDir(c.socket))
	}
	if c.dry {
		if err := sweep(c); err != nil {
			c.warn("%v", err)
		}
		return
	}
	// An in-flight sweep may have read herdr's state before the change that
	// fired this event, so only a sweep that STARTED after this event arrived
	// covers it. On contention, wait briefly and re-check; give up after
	// retryWindow (the next event self-heals a rare miss). A start time in
	// the future means the clock went back since that sweep; it covers
	// nothing, or no event would sweep until the clock caught up.
	arrival := time.Now()
	covered := func() bool {
		start := lastSweepStart(c)
		return start.After(arrival) && !start.After(time.Now())
	}
	for {
		if covered() {
			return
		}
		got, err := acquireLock(c)
		if err != nil {
			c.warn("state dir %s is not usable: %v; skipping this event", c.stateDir, err)
			return
		}
		if got {
			if !covered() {
				stampSweepStart(c)
				if err := sweep(c); err != nil {
					c.warn("%v", err)
				}
			}
			releaseLock(c)
			return
		}
		if time.Since(arrival) > retryWindow {
			c.warn("sweep lock busy for %v; skipping this event", retryWindow)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// sessionDir names the state subdirectory for one herdr session. The
// plugin's state dir is shared by every session of the user, and a sweep
// prunes state for agents it doesn't see, so each session, identified by
// its socket path, keeps its stamps, PR cache and lock apart.
func sessionDir(socket string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(socket)))
	return "session-" + hex.EncodeToString(sum[:8])
}

// ---- lock ---------------------------------------------------------------------
// One sweep at a time: an O_EXCL .lock file holding the owner's pid. The
// owner refreshes its mtime between slow steps; a stale lock is taken over.

// beforeTakeover runs between the stale check and the takeover rename; tests
// use it to stage a race there.
var beforeTakeover = func() {}

func lockPath(c *config) string { return filepath.Join(c.stateDir, ".lock") }

// lockIsStale reports whether a lock with this mtime can be taken over: it is
// untouched for lockStale, or its mtime is more than lockStale in the future.
// A future mtime means the clock went back after the last touch, and waiting
// for the clock to catch up could take any length of time. The margin keeps a
// live lock whose mtime is a little ahead (clock skew on a network
// filesystem) from being taken over; such a lock goes stale within
// 2*lockStale at most.
func lockIsStale(mtime time.Time) bool {
	age := time.Since(mtime)
	return age > lockStale || age < -lockStale
}

// acquireLock reports whether it took the lock. An error means the state dir
// can't hold a lock at all (missing, not a directory, not writable), which
// waiting won't fix; plain contention is (false, nil).
func acquireLock(c *config) (bool, error) {
	lock := lockPath(c)
	pid := []byte(strconv.Itoa(os.Getpid()))
	create := func() (bool, error) {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		_, werr := f.Write(pid)
		if cerr := f.Close(); werr != nil || cerr != nil {
			_ = os.Remove(lock) // an ownerless lock would block every sweep until stale
			return false, errors.Join(werr, cerr)
		}
		return true, nil
	}
	if err := os.MkdirAll(c.stateDir, 0o700); err != nil {
		return false, err
	}
	if got, err := create(); got || err != nil {
		return got, err
	}
	fi, err := os.Stat(lock)
	if err != nil || !lockIsStale(fi.ModTime()) {
		return false, nil
	}
	// rename is exclusive: exactly one concurrent stealer evicts the stale
	// lock, then contends for the slot like everyone else. Between our Stat
	// and Rename another stealer may have evicted it and taken a fresh lock,
	// or a stalled owner may have refreshed it. So check what we moved is the
	// same file and still stale (a fresh lock can reuse the stale one's inode
	// number); if not, put it back and back off.
	beforeTakeover()
	tomb := filepath.Join(c.stateDir, ".lock.stale-"+strconv.Itoa(os.Getpid()))
	if os.Rename(lock, tomb) != nil {
		return false, nil
	}
	if tfi, err := os.Stat(tomb); err != nil || !os.SameFile(fi, tfi) || !lockIsStale(tfi.ModTime()) {
		// ErrExist: the slot was re-taken meanwhile, nothing to restore into.
		// Anything else (e.g. no hard links here): move it back rather than
		// delete a live lock.
		if err := os.Link(tomb, lock); err != nil && !errors.Is(err, fs.ErrExist) {
			_ = os.Rename(tomb, lock)
			return false, nil
		}
		_ = os.Remove(tomb)
		return false, nil
	}
	_ = os.Remove(tomb)
	return create()
}

// The lock's mtime is its liveness signal; refresh it between slow steps.
func touchLock(c *config) {
	if c.dry {
		return
	}
	now := time.Now()
	_ = os.Chtimes(lockPath(c), now, now)
}

func releaseLock(c *config) {
	// Only remove a lock we still own: after a stall past lockStale ours may
	// have been stolen.
	if b, err := os.ReadFile(lockPath(c)); err == nil && string(b) == strconv.Itoa(os.Getpid()) {
		_ = os.Remove(lockPath(c))
	}
}

// .last-sweep's mtime records when the most recent sweep STARTED.
func lastSweepStart(c *config) time.Time {
	fi, err := os.Stat(filepath.Join(c.stateDir, ".last-sweep"))
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

func stampSweepStart(c *config) {
	p := filepath.Join(c.stateDir, ".last-sweep")
	if f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err == nil {
		f.Close()
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
}
