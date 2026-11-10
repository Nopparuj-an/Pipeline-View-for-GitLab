# Pipeline View for GitLab

A live terminal dashboard for the **latest pipeline of every project in a GitLab group**. See what's running, what failed and what's waiting for a manual click, without opening a dozen browser tabs.

<p align="center">
  <img src="docs/demo.svg" width="49%" alt="Default boxed layout">
  <img src="docs/demo-compact.svg" width="49%" alt="Compact layout (--compact)">
</p>
<p align="center"><sub>Default layout (left) and <code>--compact</code> (right)</sub></p>

- **Standard library only.** No dependencies, just Go. Runs on Linux, macOS and Windows.
- **Live.** Polls every 2s while anything is running and every 10s when everything is idle.
- **Per-stage job boxes** coloured by status, with durations and commit info.
- **Clickable.** Project, branch, pipeline, commit and job names are terminal hyperlinks (OSC 8).
- **Scriptable.** Use `--once` or pipe the output to print a snapshot and exit.

Each refresh lists the group's projects (cached for 5 minutes), then fetches the newest pipeline for each project in parallel. Jobs are only re-fetched when a pipeline is still active or has changed.

## Install

Requires Go 1.22+.

```sh
git clone <this repo>
cd pipeline
go build -o pipeline .        # Windows: go build -o pipeline.exe .
```

On Windows, run it in Windows Terminal or a recent console host (Windows 10+). Raw mode needs a real console, so Git Bash/mintty falls back to `--once` output.

Or run straight from source with `go run .`.

## Usage

Create a GitLab token with the `read_api` scope for viewing pipelines, or the `api` scope to also create tags, then:

```sh
export GITLAB_TOKEN=glpat-xxxxxxxx

./pipeline --host=gitlab.example.com --group-id=my-group
./pipeline --host=gitlab.example.com --group-id=1234 --include="web-*,api-*" --exclude="*-archive"
```

`--group-id` takes a numeric ID or the full path. Subgroups are included.

### Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--host` | `gitlab.com` | GitLab hostname. A full `http://` or `https://` URL also works. |
| `--group-id` | | Group ID or full path (required). |
| `--token` | `$GITLAB_TOKEN` | API token. |
| `--include` | | Comma-separated project name globs to show, e.g. `web-*,api-*`. |
| `--exclude` | | Comma-separated project name globs to hide. |
| `--interval` | `2s` | Refresh interval while pipelines are active. |
| `--idle-interval` | `10s` | Refresh interval when everything has finished. |
| `--compact` | off | Start in the compact (unboxed) layout. |
| `--hide-skipped` | off | Hide skipped jobs. |
| `--once` | off | Print once and exit. This is also the behaviour when output isn't a terminal. |

### Keys

| Key | Action |
| --- | --- |
| `↑` `↓` / `j` `k` | Select a pipeline |
| `g` / `G` | Jump to first / last |
| `Enter` / `o` | Open the selected pipeline in the browser |
| `t` | Tag the selected project |
| `r` | Refresh now, and poll fast for a couple of minutes (handy right after a push) |
| `f` | Hide passed pipelines |
| `s` | Toggle skipped jobs |
| `c` | Toggle compact layout |
| `q` | Quit |

### Tagging

Select a project with a pipeline and press `t`. The menu shows the latest UAT and production versions; Auto is selected by default. Press Esc to leave at any time.

- **Auto** promotes the latest unpromoted UAT tag, or creates the next patch UAT tag from `main` and then promotes it.
- **UAT** creates the next patch `uat-vX.Y.Z` tag from `main`.
- **Prod** promotes the latest unpromoted UAT tag to `vX.Y.Z`.
- **Manual** chooses UAT or Prod and starts with a suggested version. Edit it with Backspace, or Ctrl+U to clear it, to enter a different version.

The first suggested version is `v0.0.1`. Production always points to the UAT commit and requires its tag pipeline to succeed; running pipelines are awaited. Failed, canceled, blocked, skipped, or missing UAT pipelines stop promotion. If a tag pipeline does not appear within one minute, the operation stops. Confirm the tag and commit before creation. While waiting, press `b` to return to the dashboard and `t` to view progress again. Esc stops waiting but does not remove any tag already created; check GitLab if a request was in flight. GitLab tag protections and project permissions also apply.

### Status legend

| Symbol | Meaning |
| --- | --- |
| `✓` | success |
| `✕` | failed |
| `!` | failed, but allowed to fail (shown as "passed with warnings") |
| spinner | running |
| `○` | pending / created |
| `►` | manual, waiting for a click |
| `⊘` | canceled |
| `»` | skipped |
