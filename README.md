# herdr Agent Rows

A [herdr](https://herdr.dev) plugin for the Agent panel in herdr's sidebar.
It shows each agent on one line: its workspace name, the branch or PR it is
working on, and the time of its last state change. A second line shows the
terminal title, but only when it differs from the workspace name.

```
● api · feat/rate-limit · idle 13:40
   Add rate limiting to the login endpoint
✓ notes · done 14:02
◐ web · #42
```

It can also group agents. In setups such as agentic teams, where one agent
coordinates others, panes tagged with `role` and `lead` tokens show as a
lead with its workers indented beneath it. This is optional; see
[Grouping agents under a lead](#grouping-agents-under-a-lead).

## Why

The default Agent rows spend three lines per agent on about one and a half
facts: the workspace name, a terminal title that often repeats it, and
`claude · idle`. They don't show which branch or PR an agent is on, or how
long ago it finished.

herdr's sidebar config can't choose what a row shows based on another
token's value (style rules only see a token's own value). This plugin works
out each row itself and reports the result as `$ar_*` pane tokens for the
sidebar config to render.

## Requirements

- herdr 0.9.1 or later, on macOS or Linux.
- Go 1.26 or later. herdr builds the binary at install time. `go.mod` names
  toolchain go1.26.8, so an older 1.26 release downloads it on first build
  unless `GOTOOLCHAIN=local` is set. The plugin uses only the standard
  library.
- Optional: `gh`, signed in. Without it, rows show branch names but not PR
  numbers.
- Optional: `git`. It is only run for repository layouts the plugin can't
  read from files directly, such as reftable.

## Install

```sh
herdr plugin install ryanlewis/herdr-agent-rows
```

In an interactive terminal, herdr previews the source and the build command
(`go build -o agent-rows ./cmd/agent-rows`) and asks before running it. Pin
a revision with `--ref`.

The plugin reports tokens but changes nothing on screen until you add them
to the sidebar config below.

To remove it:

```sh
herdr plugin uninstall io.rlew.agent-rows
```

## Sidebar config

In your herdr `config.toml` (see
[UI and sidebar](https://herdr.dev/docs/configuration/#ui-and-sidebar)):

```toml
[ui.sidebar.agents]
rows = [
  ["state_icon", { token = "$ar_name", bold = true }, "$ar_git", "$ar_state",
   { token = "$ar_wname", dim = true }, { token = "$ar_wgit", dim = true }, { token = "$ar_wstate", dim = true }],
  ["$ar_title"],
]
```

The `$ar_w*` entries are only set for workers when you use grouping.
Otherwise they stay empty and nothing is shown for them.

## What the rows show

Each agent's row shows:

- **Workspace name**, in bold.
- **Branch or PR**, only when the checkout is off its default branch
  (`origin/HEAD`, else `main` or `master`). If the branch has a PR, the PR
  replaces the branch name: `#42`, `#42 merged`, `#42 closed`.
- **State time**, recorded when the state changes: `done 14:02`,
  `blocked 14:05`, `idle 14:02`. Nothing is shown while an agent works,
  because the icon already says so. `done` and `idle` count as one state,
  so looking at a finished agent keeps its finish time. An agent first seen
  partway through a state shows the state without a time.
- **Terminal title**, on a second line, only when it differs from the
  workspace name.

## Grouping agents under a lead

Grouping is for setups where one agent coordinates others, such as an
agentic team with a lead and workers. It turns on only when something tags
panes with `role` and `lead` tokens. Untagged agents keep the rows
described above.

```
● notes · lead · 3 workers, 1 blocked
◐   bun-pins
✓   wide-layout · #42 · done 14:02
●   abc-migration · blocked 14:05
```

- **Lead** (pane token `role=lead`): the workspace name, branch or PR, and
  a count of its live workers, with how many are blocked.
- **Worker** (`role=worker` and `lead=<lead pane id>`): one dimmed line
  with the worker's name indented under the lead, then branch or PR, then
  state time. The name is the herdr agent name, else the pane label, else
  the terminal title, else the pane id. Workers are named rather than
  shown by workspace, because workers in one workspace would otherwise all
  show the same name.
- A worker whose lead pane is gone, no longer runs an agent, or is no
  longer tagged `role=lead` goes back to an ordinary row.

The plugin does not create or manage groups. Whatever starts the agents
(you, a script, or an agent that starts others) tags the panes. Inside the
lead's pane, `$HERDR_PANE_ID` is the lead's pane id:

```sh
# the lead
herdr pane report-metadata "$HERDR_PANE_ID" --source team --token role=lead

# each worker
herdr pane report-metadata <worker-pane-id> --source team \
  --token role=worker --token lead=<lead-pane-id>
```

- `--source` can be any id; herdr stores tokens per pane, not per source.
- Use the lead's pane id (from `herdr pane list` or `$HERDR_PANE_ID`), not
  a name, because names can change.
- A pane can't be its own lead.
- Rows update on the next hooked event from any pane, such as an agent
  changing state or a pane taking focus. Setting a token is not itself an
  event the plugin hears.

To untag a pane:

```sh
herdr pane report-metadata <pane-id> --source team --clear-token role --clear-token lead
```

Keep `agent_panel_sort = "spaces"` (the default), so workers stay next to
their lead: herdr lists agents in workspace, tab, pane order, so workers in
later tabs of the lead's workspace appear under it. Under `priority` sort,
rows are ordered by state instead.

herdr trims leading whitespace from token values, and a token placed before
`state_icon` gets a ` · ` separator, so a worker row can't be indented
before its icon. The worker name is indented after the icon instead, with
two U+2800 (braille blank) characters. herdr doesn't count them as
whitespace, so they survive and render as blank cells.

## How it works

herdr runs `agent-rows` on each hooked event. Every run sweeps all agents
and ignores the event payload, so a missed event is corrected by the next
one. A sweep:

1. asks herdr's socket API (`HERDR_SOCKET_PATH`) for one
   `session.snapshot`: agents with status, cwd, title and pane tokens,
   workspace labels and pane labels;
2. records state changes in the plugin's state file, keyed by terminal id;
3. reads the branch and default branch from the git files, once per
   distinct cwd: `HEAD` (following a worktree's `gitdir:` file) and
   `refs/remotes/origin/HEAD` in the common git dir. Only small regular
   files are read. A layout it doesn't recognise, such as reftable, falls
   back to `git symbolic-ref`;
4. runs `gh pr list --head=<branch> --state all --json number,state` in
   the agent's cwd for each branch whose agent changed state since its last
   lookup. Of several PRs from the branch, an open one is shown, else the
   most recent. At most 4 lookups run per sweep, in parallel, each stopped
   after 4 seconds. A missing, failing or slow `gh` is cached as "no PR"
   until the next state change, and the row shows the branch;
5. reports only the tokens that changed, one `pane.report_metadata` request
   per changed pane, under source `io.rlew.agent-rows`. A sweep with no
   changes makes no reports.

A sweep takes a few milliseconds, most of it process start, plus any `gh`
calls.

Sweeps run one at a time. A `.lock` file in the state directory holds the
running sweep's pid, and a lock untouched for 30 seconds is taken over. A
`.last-sweep` file records when the latest sweep started, so an event that
a newer sweep already covered doesn't start another.

Hooked events: `pane.agent_detected`, `pane.agent_status_changed`,
`pane.focused`, `pane.created`, `pane.closed`, `tab.closed`,
`workspace.closed`, `pane.exited` and `workspace.renamed`. Closing a tab or
workspace doesn't fire `pane.closed` for the panes inside, hence the
separate hooks. `pane.updated` is not hooked, because herdr fires it on
every terminal title change and titles change constantly while an agent
works.

### What it reads, runs and writes

- Reads: herdr's socket, and git metadata files under each agent's cwd.
- Runs: `gh` and, for unusual layouts, `git`, both with fixed arguments.
  `gh` makes network calls to GitHub; the plugin itself does not.
- Writes: pane tokens through the socket, and `state.json`, `.lock` and
  `.last-sweep` (plus short-lived `state.json.tmp-<pid>` and
  `.lock.stale-<pid>` files) in `HERDR_PLUGIN_STATE_DIR`
  (`~/.local/state/herdr/plugins/io.rlew.agent-rows/` by default).
  `state.json` is written with mode 0600. It holds terminal ids, state
  times, checkout paths, branch names and PR numbers. It is safe to delete;
  times then reappear after each agent's next state change.

## Tokens

| Token | Ungrouped agent | Lead | Worker |
| --- | --- | --- | --- |
| `ar_name` | workspace | workspace | |
| `ar_git` | branch or PR | branch or PR | |
| `ar_state` | `done 14:02` | worker count | |
| `ar_title` | title, if not the workspace name | | |
| `ar_wname` | | | `⠀⠀<name>` |
| `ar_wgit` | | | branch or PR |
| `ar_wstate` | | | `done 14:02` |

Workers get their own `ar_w*` tokens so the config can dim the whole row.
Style rules only see a token's own value, so they can't tell a worker's
branch from anyone else's.

The plugin reads `role` and `lead` and never writes them.

## Limitations

- A PR merged or closed while its agent sits idle shows only after that
  agent's next state change.
- Without the plugin, the rows in the sidebar config above show only the
  state icon.
- Pane tokens are one map per pane, shared by every source. Another tool
  writing `ar_*`, `role` or `lead` would clash, and any process that can
  reach herdr's socket can set `role` and `lead`.
- With grouping: herdr does not restore pane tokens after a `herdr server`
  restart. The plugin reports its own tokens again on the next sweep, but
  `role` and `lead` are gone, so workers show as ordinary rows until the
  panes are tagged again.
- With grouping: if a worker's tab is moved in front of the lead's tab, the
  worker shows above its lead.

## Development

`herdr plugin link` skips the build step, so build the binary yourself
first, and again after each change:

```sh
go build -o agent-rows ./cmd/agent-rows
herdr plugin link .      # later: herdr plugin unlink io.rlew.agent-rows
```

Each run's output is in `herdr plugin log list --plugin io.rlew.agent-rows`.

## Test

```sh
go vet ./... && go test ./...
./agent-rows --dry-run    # against a running herdr: prints each pane's tokens, reports nothing
```

The tests run offline against a fake herdr socket, a fake `gh` and
throwaway git repositories. They need `git` and `sh` on the path; the FIFO
test is skipped without `mkfifo`.

CI runs `gofmt`, `go vet`, the tests and a build on Linux and macOS, and
[govulncheck](https://go.dev/doc/security/vuln/) on pushes to `main` and on
pull requests.

## License

MIT
