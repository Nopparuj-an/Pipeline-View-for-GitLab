// pipeline: live terminal view of the latest GitLab pipeline for each project in a group.
//
// Standard library only. Run straight from source:
//
//	go run . --group-id=my-group --host=gitlab.example.com --include="web-*,api-*"
//
// Keys: ↑/↓ or j/k select, Enter/o open pipeline, r refresh now (and poll fast for a while),
// f hide passed pipelines, s toggle skipped jobs, c compact layout, q quit.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

const (
	projectsTTL = 5 * time.Minute // how often the project list itself is re-fetched
	boostFor    = 2 * time.Minute // fast polling after pressing r (e.g. right after a push)
	newBadgeFor = time.Minute     // how long a freshly appeared pipeline is marked NEW
	durW        = 6               // width of job durations like "12m03s"
)

const (
	reset   = "\x1b[0m"
	bold    = "\x1b[1m"
	grey    = "\x1b[90m"
	red     = "\x1b[91m"
	green   = "\x1b[92m"
	yellow  = "\x1b[93m"
	blue    = "\x1b[94m"
	magenta = "\x1b[95m"
	cyan    = "\x1b[96m"
	badge   = "\x1b[30;103m"
)

// ---------- config ----------

type config struct {
	host, token, group string
	include, exclude   []string
	hideSkipped        bool
	compact            bool
	fast, idle         time.Duration
	once               bool
}

func parseFlags() *config {
	cfg := &config{}
	var include, exclude string
	flag.StringVar(&cfg.host, "host", "gitlab.com", "hostname of the GitLab instance")
	flag.StringVar(&cfg.token, "token", "", "GitLab API token (default: $GITLAB_TOKEN)")
	flag.StringVar(&cfg.group, "group-id", "", "group ID or full path")
	flag.StringVar(&include, "include", "", "comma-separated project-name patterns to include, e.g. 'web-*,api-*'")
	flag.StringVar(&exclude, "exclude", "", "comma-separated project-name patterns to exclude")
	flag.BoolVar(&cfg.hideSkipped, "hide-skipped", false, "hide skipped jobs (toggle with s)")
	flag.DurationVar(&cfg.fast, "interval", 2*time.Second, "refresh interval while pipelines are active")
	flag.DurationVar(&cfg.idle, "idle-interval", 10*time.Second, "refresh interval when everything is finished")
	flag.BoolVar(&cfg.compact, "compact", false, "start in compact (unboxed) layout (toggle with c)")
	flag.BoolVar(&cfg.once, "once", false, "print once and exit (also used when output is not a terminal)")
	flag.Bool("watch", true, "ignored; watching is the default (kept for compatibility)")
	flag.Parse()

	if cfg.token == "" {
		cfg.token = os.Getenv("GITLAB_TOKEN")
	}
	if cfg.token == "" || cfg.group == "" {
		fmt.Fprintln(os.Stderr, "pipeline: --group-id and a token (--token or $GITLAB_TOKEN) are required")
		flag.Usage()
		os.Exit(2)
	}
	cfg.include, cfg.exclude = splitPatterns(include), splitPatterns(exclude)
	return cfg
}

func splitPatterns(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func matchAny(name string, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

// ---------- GitLab API ----------

type Project struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	WebURL string `json:"web_url"`
}

type Pipeline struct {
	ID        int       `json:"id"`
	IID       int       `json:"iid"`
	Status    string    `json:"status"`
	Ref       string    `json:"ref"`
	SHA       string    `json:"sha"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Job struct {
	ID           int        `json:"id"`
	Name         string     `json:"name"`
	Stage        string     `json:"stage"`
	Status       string     `json:"status"`
	WebURL       string     `json:"web_url"`
	AllowFailure bool       `json:"allow_failure"`
	Duration     *float64   `json:"duration"`
	StartedAt    *time.Time `json:"started_at"`
	Commit       struct {
		ShortID    string `json:"short_id"`
		Title      string `json:"title"`
		AuthorName string `json:"author_name"`
	} `json:"commit"`
	User *struct {
		Username string `json:"username"`
	} `json:"user"`
}

type client struct {
	base, token string
	http        *http.Client
}

func newClient(cfg *config) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 16 // keep connections warm for the parallel requests
	base := "https://" + cfg.host
	if strings.HasPrefix(cfg.host, "http://") || strings.HasPrefix(cfg.host, "https://") {
		base = cfg.host // explicit scheme, e.g. http://localhost:8080 for the demo server
	}
	return &client{
		base:  base + "/api/v4",
		token: cfg.token,
		http:  &http.Client{Transport: tr, Timeout: 15 * time.Second},
	}
}

// get decodes a JSON response into v and returns the X-Next-Page header.
func (c *client) get(ctx context.Context, path string, v any) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Private-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the noisy "Get <url>:" prefix
		}
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		var e struct {
			Message any    `json:"message"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil {
			if e.Message != nil {
				msg = fmt.Sprint(e.Message)
			} else if e.Error != "" {
				msg = e.Error
			}
		}
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return "", fmt.Errorf("bad response: %w", err)
	}
	return resp.Header.Get("X-Next-Page"), nil
}

func listProjects(ctx context.Context, c *client, cfg *config) ([]Project, error) {
	var out []Project
	for page := "1"; page != ""; {
		var ps []Project
		next, err := c.get(ctx, fmt.Sprintf("/groups/%s/projects?include_subgroups=true&simple=true&archived=false&per_page=100&page=%s",
			url.PathEscape(cfg.group), page), &ps)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			if len(cfg.include) > 0 && !matchAny(p.Name, cfg.include) {
				continue
			}
			if matchAny(p.Name, cfg.exclude) {
				continue
			}
			out = append(out, p)
		}
		page = next
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// known is what we already have for a project, so unchanged finished pipelines skip the jobs request.
type known struct {
	pipelineID int
	updatedAt  time.Time
	status     string
	haveJobs   bool
}

type projectResult struct {
	fetched     bool      // latest pipeline was fetched; pipeline may still be nil (project has none)
	pipeline    *Pipeline //
	jobs        []Job     //
	jobsFetched bool      //
	err         error     //
}

type fetchResult struct {
	projects []Project // non-nil when the project list was refreshed
	results  map[int]projectResult
	err      error
	at       time.Time
}

func fetch(c *client, cfg *config, projects []Project, refreshProjects bool, prev map[int]known) fetchResult {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res := fetchResult{results: map[int]projectResult{}}
	if refreshProjects {
		if ps, err := listProjects(ctx, c, cfg); err != nil {
			res.err = err
		} else {
			res.projects, projects = ps, ps
		}
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, p := range projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := fetchProject(ctx, c, p, prev[p.ID])
			mu.Lock()
			res.results[p.ID] = r
			mu.Unlock()
		}()
	}
	wg.Wait()
	res.at = time.Now()
	return res
}

func fetchProject(ctx context.Context, c *client, p Project, k known) projectResult {
	var r projectResult
	var pls []Pipeline
	if _, err := c.get(ctx, fmt.Sprintf("/projects/%d/pipelines?per_page=1&order_by=id&sort=desc", p.ID), &pls); err != nil {
		r.err = err
		return r
	}
	r.fetched = true
	if len(pls) == 0 {
		return r
	}
	pl := pls[0]
	r.pipeline = &pl
	if k.haveJobs && k.pipelineID == pl.ID && k.updatedAt.Equal(pl.UpdatedAt) && k.status == pl.Status && !isActive(pl.Status) {
		return r // finished and unchanged: keep the jobs we have
	}
	var jobs []Job
	if _, err := c.get(ctx, fmt.Sprintf("/projects/%d/pipelines/%d/jobs?per_page=100", p.ID, pl.ID), &jobs); err != nil {
		r.err = err
		return r
	}
	r.jobs, r.jobsFetched = jobs, true
	return r
}

func isActive(status string) bool {
	switch status {
	case "created", "waiting_for_resource", "preparing", "pending", "running", "canceling", "scheduled":
		return true
	}
	return false
}

// ---------- model ----------

type entry struct {
	project  Project
	pipeline *Pipeline
	jobs     []Job
	jobsFor  int // pipeline ID the jobs belong to
	err      string
	newUntil time.Time
}

type model struct {
	cfg *config
	c   *client

	entries    map[int]*entry
	projects   []Project
	projectsAt time.Time
	loaded     bool

	lastFetch   time.Time
	lastErr     string
	failures    int
	fetching    bool
	fetchStart  time.Time
	manual      bool // a user-triggered refresh is in flight: show the spinner right away
	queued      bool
	queuedForce bool
	nextAt      time.Time
	boostUntil  time.Time

	selected    int // project ID
	offset      int
	hidePassed  bool
	compact     bool
	hideSkipped bool
	flash       string
	flashUntil  time.Time
	w, h        int
}

func (m *model) merge(r fetchResult) {
	if r.projects != nil {
		m.projects, m.projectsAt = r.projects, r.at
		keep := map[int]bool{}
		for _, p := range r.projects {
			keep[p.ID] = true
			if e := m.entries[p.ID]; e != nil {
				e.project = p
			} else {
				m.entries[p.ID] = &entry{project: p}
			}
		}
		for id := range m.entries {
			if !keep[id] {
				delete(m.entries, id)
			}
		}
	}

	var projectErr error
	failed := 0
	for id, pr := range r.results {
		e := m.entries[id]
		if e == nil {
			continue
		}
		e.err = ""
		if pr.err != nil {
			e.err, projectErr = shortErr(pr.err), pr.err
			failed++
		}
		if pr.fetched {
			if pr.pipeline != nil && m.loaded && (e.pipeline == nil || pr.pipeline.ID != e.pipeline.ID) {
				e.newUntil = r.at.Add(newBadgeFor)
			}
			e.pipeline = pr.pipeline
		}
		if pr.jobsFetched {
			e.jobs, e.jobsFor = pr.jobs, pr.pipeline.ID
		}
	}

	m.lastFetch = r.at
	switch {
	case r.err != nil:
		m.lastErr = shortErr(r.err)
		m.failures++
	case projectErr != nil:
		m.lastErr = fmt.Sprintf("%d/%d failed: %s", failed, len(r.results), shortErr(projectErr))
		m.failures++
	default:
		m.lastErr, m.failures = "", 0
	}
	if m.projects != nil {
		m.loaded = true
	}
}

func shortErr(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}

func (m *model) known() map[int]known {
	k := map[int]known{}
	for id, e := range m.entries {
		if e.pipeline != nil {
			k[id] = known{e.pipeline.ID, e.pipeline.UpdatedAt, e.pipeline.Status, e.jobsFor == e.pipeline.ID}
		}
	}
	return k
}

func (m *model) anyActive() bool {
	for _, e := range m.entries {
		if e.pipeline != nil && isActive(e.pipeline.Status) {
			return true
		}
	}
	return false
}

func (m *model) interval(now time.Time) time.Duration {
	if m.failures > 0 {
		return min(m.cfg.fast<<min(m.failures, 5), time.Minute)
	}
	if now.Before(m.boostUntil) || m.anyActive() {
		return m.cfg.fast
	}
	return m.cfg.idle
}

// visible returns projects to draw (newest pipeline first) and the names of projects without pipelines.
func (m *model) visible() (shown []*entry, empty []string) {
	for _, p := range m.projects {
		e := m.entries[p.ID]
		switch {
		case e.pipeline == nil && e.err == "":
			empty = append(empty, p.Name)
		case m.hidePassed && e.pipeline != nil && e.pipeline.Status == "success":
		default:
			shown = append(shown, e)
		}
	}
	updated := func(e *entry) time.Time {
		if e.pipeline == nil {
			return time.Time{}
		}
		return e.pipeline.UpdatedAt
	}
	sort.SliceStable(shown, func(i, j int) bool { return updated(shown[i]).After(updated(shown[j])) })
	return shown, empty
}

func (m *model) move(delta int) {
	shown, _ := m.visible()
	if len(shown) == 0 {
		return
	}
	i := m.selectedIndex(shown)
	i = max(0, min(len(shown)-1, i+delta))
	m.selected = shown[i].project.ID
}

func (m *model) selectedIndex(shown []*entry) int {
	for i, e := range shown {
		if e.project.ID == m.selected {
			return i
		}
	}
	if len(shown) > 0 {
		m.selected = shown[0].project.ID
	}
	return 0
}

func (m *model) setFlash(s string) {
	m.flash, m.flashUntil = s, time.Now().Add(2*time.Second)
}

func (m *model) open() {
	shown, _ := m.visible()
	if len(shown) == 0 {
		return
	}
	e := shown[m.selectedIndex(shown)]
	target := e.project.WebURL
	if e.pipeline != nil {
		target = e.pipeline.WebURL
	}
	cmd := exec.Command("xdg-open", target)
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	}
	if err := cmd.Start(); err != nil {
		m.setFlash("open failed: " + err.Error())
		return
	}
	go cmd.Wait()
	m.setFlash("opened " + e.project.Name)
}

// ---------- rendering ----------

// line builds one terminal line, tracking visible width and clipping at max.
type line struct {
	b      strings.Builder
	w, max int
}

func newLine(max int) *line { return &line{max: max} }

func (l *line) add(style, text string) { l.link(style, "", text) }

func (l *line) link(style, href, text string) {
	room := l.max - l.w
	if room <= 0 || text == "" {
		return
	}
	text = truncate(text, room)
	l.b.WriteString(style)
	if href != "" {
		l.b.WriteString("\x1b]8;;" + href + "\x1b\\") // OSC 8 hyperlink
	}
	l.b.WriteString(text)
	if href != "" {
		l.b.WriteString("\x1b]8;;\x1b\\")
	}
	if style != "" {
		l.b.WriteString(reset)
	}
	l.w += width(text)
}

func (l *line) padTo(col int) {
	if col > l.w {
		l.add("", strings.Repeat(" ", col-l.w))
	}
}

// fill repeats ch (a single-width rune) until the line reaches col.
func (l *line) fill(style, ch string, col int) {
	if col > l.w {
		l.add(style, strings.Repeat(ch, col-l.w))
	}
}

// append adds an already-rendered line; o must have been built to fit.
func (l *line) append(o *line) {
	l.b.WriteString(o.String())
	l.w += o.w
}

func (l *line) String() string { return l.b.String() }

func runeWidth(r rune) int {
	switch {
	case r < 32, r == 0xFE0F, unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case r >= 0x1100 && r <= 0x115F, r >= 0x2E80 && r <= 0xA4CF, r >= 0xAC00 && r <= 0xD7A3,
		r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F, r >= 0xFF00 && r <= 0xFF60,
		r >= 0xFFE0 && r <= 0xFFE6, r >= 0x1F300 && r <= 0x1F64F, r >= 0x1F680 && r <= 0x1F6FF,
		r >= 0x1F900 && r <= 0x1F9FF, r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func width(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if width(s) <= max {
		return s
	}
	var b strings.Builder
	w := 0
	for _, r := range s {
		rw := runeWidth(r)
		if w+rw > max-1 {
			break
		}
		b.WriteRune(r)
		w += rw
	}
	return b.String() + "…"
}

func spinner(now time.Time) string {
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return frames[now.UnixMilli()/100%int64(len(frames))]
}

func icon(status string, allowFailure bool, now time.Time) (string, string) {
	switch status {
	case "success":
		return "✓", green
	case "failed":
		if allowFailure {
			return "!", yellow
		}
		return "✕", red
	case "running":
		return spinner(now), blue
	case "pending", "created", "waiting_for_resource", "preparing", "scheduled":
		return "○", ""
	case "manual":
		return "►", ""
	case "canceled", "canceling":
		return "⊘", grey
	case "skipped":
		return "»", grey
	}
	return "?", ""
}

func ago(d time.Duration) string {
	s := max(0, int(d.Seconds()))
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm", s/60)
	case s < 48*3600:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dd", s/86400)
}

func dur(d time.Duration) string {
	s := max(0, int(d.Round(time.Second).Seconds()))
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s/60%60)
}

func jobDur(j Job, now time.Time) string {
	if j.Duration != nil && *j.Duration > 0 {
		return dur(time.Duration(*j.Duration * float64(time.Second)))
	}
	if j.StartedAt != nil && isActive(j.Status) {
		return dur(now.Sub(*j.StartedAt))
	}
	return ""
}

func refLink(p Project, ref string) (text, href string) {
	if rest, ok := strings.CutPrefix(ref, "refs/merge-requests/"); ok {
		iid, _, _ := strings.Cut(rest, "/")
		return "!" + iid, p.WebURL + "/-/merge_requests/" + iid
	}
	return ref, p.WebURL + "/-/tree/" + (&url.URL{Path: ref}).EscapedPath()
}

type stage struct {
	name  string
	first int
	jobs  []Job
}

func groupStages(jobs []Job, hideSkipped bool) []*stage {
	byName := map[string]*stage{}
	var out []*stage
	for _, j := range jobs {
		if hideSkipped && j.Status == "skipped" {
			continue
		}
		s := byName[j.Stage]
		if s == nil {
			s = &stage{name: j.Stage, first: j.ID}
			byName[j.Stage] = s
			out = append(out, s)
		}
		s.first = min(s.first, j.ID)
		s.jobs = append(s.jobs, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].first < out[b].first }) // jobs are created in stage order
	for _, s := range out {
		sort.Slice(s.jobs, func(a, b int) bool { return s.jobs[a].Name < s.jobs[b].Name })
	}
	return out
}

// stageStatus summarises a stage as the status/allowFailure pair its header is coloured by.
func stageStatus(jobs []Job) (string, bool) {
	has := map[string]bool{}
	warn := false
	for _, j := range jobs {
		if j.Status == "failed" && j.AllowFailure {
			warn = true
			continue
		}
		has[j.Status] = true
	}
	for _, s := range []string{"failed", "running", "pending", "created", "waiting_for_resource", "preparing", "scheduled", "manual", "canceling", "canceled"} {
		if has[s] {
			return s, false
		}
	}
	if warn {
		return "failed", true
	}
	if has["success"] {
		return "success", false
	}
	return "skipped", false
}

func pipelineState(pl *Pipeline, jobs []Job, now time.Time) (ic, color, label string) {
	ic, color = icon(pl.Status, false, now)
	label = strings.ReplaceAll(pl.Status, "_", " ")
	if pl.Status == "success" {
		for _, j := range jobs {
			if j.Status == "failed" && j.AllowFailure {
				return "!", yellow, "passed with warnings"
			}
		}
	}
	return ic, color, label
}

func commitLine(l *line, p Project, pl *Pipeline, jobs []Job) {
	c := jobs[0].Commit
	who := c.AuthorName
	if jobs[0].User != nil {
		who = "@" + jobs[0].User.Username
	}
	title, _, _ := strings.Cut(c.Title, "\n")
	l.link(grey, p.WebURL+"/-/commit/"+pl.SHA, c.ShortID)
	l.add(grey, " "+title+" · "+who)
}

// renderCard draws a project as a rounded box with the pipeline summary in its top border
// and one small box per stage, coloured by the stage's status.
func (m *model) renderCard(e *entry, selected bool, w int, now time.Time) []string {
	bc := grey
	if selected {
		bc = bold + cyan
	}
	iw := w - 4 // inside "│ " … " │"
	var lines []string
	row := func(c *line) {
		c.padTo(iw)
		o := newLine(w)
		o.add(bc, "│ ")
		o.append(c)
		o.add(bc, " │")
		lines = append(lines, o.String())
	}

	pl := e.pipeline
	var jobs []Job
	if pl != nil && e.jobsFor == pl.ID {
		jobs = e.jobs
	}

	// Top border: ╭─ ✓ name  ref  #iid ───── status · ago  NEW ─╮
	right := newLine(w / 2)
	if pl != nil {
		_, color, label := pipelineState(pl, jobs, now)
		right.add(color, label)
		right.add(grey, " · "+ago(now.Sub(pl.UpdatedAt))+" ago")
		if isActive(pl.Status) {
			right.add(grey, " · "+dur(now.Sub(pl.CreatedAt))+" elapsed")
		}
		if now.Before(e.newUntil) {
			right.add("", " ")
			right.add(badge, " NEW ")
		}
	}
	left := newLine(max(0, w-3-1-1-right.w-1-3))
	if pl == nil {
		left.link(bold, e.project.WebURL, e.project.Name)
	} else {
		ic, color, _ := pipelineState(pl, jobs, now)
		refText, refURL := refLink(e.project, pl.Ref)
		left.add(color, ic+" ")
		left.link(bold+color, pl.WebURL, e.project.Name)
		left.add("", "  ")
		left.link(magenta, refURL, refText)
		left.add("", "  ")
		left.link(grey, pl.WebURL, fmt.Sprintf("#%d", pl.IID))
	}
	t := newLine(w)
	t.add(bc, "╭─ ")
	t.append(left)
	t.add("", " ")
	t.fill(bc, "─", w-3-right.w-1)
	t.add("", " ")
	t.append(right)
	t.add(bc, " ─╮")
	lines = append(lines, t.String())

	if e.err != "" {
		c := newLine(iw)
		c.add(red, "✕ "+e.err)
		row(c)
	}
	if len(jobs) > 0 {
		c := newLine(iw)
		commitLine(c, e.project, pl, jobs)
		row(c)
	}

	stages := groupStages(jobs, m.hideSkipped)
	need := 0
	for _, s := range stages {
		need = max(need, width(s.name)+6) // ┌─ name ─┐
		for _, j := range s.jobs {
			need = max(need, width(j.Name)+7+durW) // │ ✓ name  12m03s │
		}
	}
	// As many stage boxes per row as fit, then stretch them to use the full width.
	cols := max(1, min(len(stages), (iw+1)/(min(max(need, 22), iw)+1)))
	colW := (iw+1)/cols - 1

	for first := 0; first < len(stages); first += cols {
		chunk := stages[first:min(first+cols, len(stages))]
		rows := 0
		for _, s := range chunk {
			rows = max(rows, len(s.jobs))
		}
		for r := 0; r < rows+2; r++ {
			c := newLine(iw)
			for ci, s := range chunk {
				x := ci * (colW + 1)
				c.padTo(x)
				st, allow := stageStatus(s.jobs)
				_, sc := icon(st, allow, now)
				if sc == "" {
					sc = grey
				}
				switch {
				case r == 0:
					c.add(sc, "┌─ ")
					c.add(bold+sc, truncate(s.name, colW-6))
					c.add("", " ")
					c.fill(sc, "─", x+colW-1)
					c.add(sc, "┐")
				case r == rows+1:
					c.add(sc, "└")
					c.fill(sc, "─", x+colW-1)
					c.add(sc, "┘")
				default:
					c.add(sc, "│ ")
					if r-1 < len(s.jobs) {
						j := s.jobs[r-1]
						ji, jc := icon(j.Status, j.AllowFailure, now)
						c.add(jc, ji+" ")
						c.link(jc, j.WebURL, truncate(j.Name, colW-7-durW))
						if d := jobDur(j, now); d != "" {
							c.padTo(x + colW - 2 - width(d))
							c.add(grey, d)
						}
					}
					c.padTo(x + colW - 1)
					c.add(sc, "│")
				}
			}
			row(c)
		}
	}

	b := newLine(w)
	b.add(bc, "╰")
	b.fill(bc, "─", w-1)
	b.add(bc, "╯")
	return append(lines, b.String())
}

func (m *model) renderEntry(e *entry, selected bool, w int, now time.Time) []string {
	var lines []string
	start := func() *line {
		l := newLine(w)
		if selected {
			l.add(cyan, "▌ ")
		} else {
			l.add("", "  ")
		}
		return l
	}

	pl := e.pipeline
	l := start()
	if pl == nil {
		l.link(bold, e.project.WebURL, e.project.Name)
		l.add(red, "  ✕ "+e.err)
		return append(lines, l.String())
	}
	var jobs []Job
	if e.jobsFor == pl.ID {
		jobs = e.jobs
	}

	ic, color, label := pipelineState(pl, jobs, now)
	refText, refURL := refLink(e.project, pl.Ref)
	l.add(color, ic+" ")
	l.link(bold+color, pl.WebURL, e.project.Name)
	l.add("", "  ")
	l.link(magenta, refURL, refText)
	l.add("", "  ")
	l.link(grey, pl.WebURL, fmt.Sprintf("#%d", pl.IID))
	l.add("", "  ")
	l.add(color, label)
	l.add(grey, "  "+ago(now.Sub(pl.UpdatedAt))+" ago")
	if isActive(pl.Status) {
		l.add(grey, " · "+dur(now.Sub(pl.CreatedAt))+" elapsed")
	}
	if now.Before(e.newUntil) {
		l.add("", "  ")
		l.add(badge, " NEW ")
	}
	if e.err != "" {
		l.add(red, "  ✕ "+e.err)
	}
	lines = append(lines, l.String())

	if len(jobs) > 0 {
		l = start()
		l.add("", "  ")
		commitLine(l, e.project, pl, jobs)
		lines = append(lines, l.String())
	}

	stages := groupStages(jobs, m.hideSkipped)
	if len(stages) == 0 {
		return lines
	}
	nameW := 0
	for _, s := range stages {
		nameW = max(nameW, width(s.name)+2)
		for _, j := range s.jobs {
			nameW = max(nameW, width(j.Name))
		}
	}
	avail := w - 2
	colW := min(max(nameW+2+1+durW, 18), 42, avail)
	cols := max(1, (avail+1)/(colW+1))

	for first := 0; first < len(stages); first += cols {
		chunk := stages[first:min(first+cols, len(stages))]
		rows := 0
		for _, s := range chunk {
			rows = max(rows, len(s.jobs))
		}
		for r := 0; r <= rows; r++ {
			l := start()
			for ci, s := range chunk {
				x := 2 + ci*(colW+1)
				l.padTo(x)
				if r == 0 {
					st, allow := stageStatus(s.jobs)
					_, sc := icon(st, allow, now)
					if sc == "" {
						sc = grey
					}
					title := "─ " + s.name + " "
					l.add(sc, truncate(title, colW)+strings.Repeat("─", max(0, colW-width(title))))
					continue
				}
				if r-1 >= len(s.jobs) {
					continue
				}
				j := s.jobs[r-1]
				ji, jc := icon(j.Status, j.AllowFailure, now)
				l.add(jc, ji+" ")
				l.link(jc, j.WebURL, truncate(j.Name, colW-2-1-durW))
				if d := jobDur(j, now); d != "" {
					l.padTo(x + colW - width(d))
					l.add(grey, d)
				}
			}
			lines = append(lines, l.String())
		}
	}
	return lines
}

// content renders the project list; selStart/selEnd bound the selected block (end exclusive).
func (m *model) content(w int, now time.Time, interactive bool) (lines []string, selStart, selEnd int) {
	if !m.loaded {
		msg := grey + "  loading…" + reset
		if m.lastErr != "" {
			msg = red + "  ✕ " + m.lastErr + reset
		}
		return []string{msg}, 0, 1
	}
	shown, empty := m.visible()
	if len(shown) == 0 {
		msg := "  no pipelines"
		if m.hidePassed {
			msg = "  nothing but passed pipelines (f to show them)"
		}
		lines = append(lines, grey+msg+reset)
	}
	m.selectedIndex(shown)
	for i, e := range shown {
		if i > 0 && m.compact {
			lines = append(lines, "")
		}
		sel := interactive && e.project.ID == m.selected
		if sel {
			selStart = len(lines)
		}
		if m.compact {
			lines = append(lines, m.renderEntry(e, sel, w, now)...)
		} else {
			lines = append(lines, m.renderCard(e, sel, w-1, now)...)
		}
		if sel {
			selEnd = len(lines)
		}
	}
	if len(empty) > 0 {
		l := newLine(w)
		l.add(grey, "  no pipelines: "+strings.Join(empty, ", "))
		lines = append(lines, "", l.String())
	}
	return lines, selStart, selEnd
}

func (m *model) frame(now time.Time) string {
	w, h := m.w, m.h
	body := max(1, h-2)
	lines, selStart, selEnd := m.content(w, now, true)

	// Scroll so the selected block is visible (its top wins if it is taller than the screen).
	m.offset = min(m.offset, max(0, len(lines)-body))
	if selEnd > m.offset+body {
		m.offset = selEnd - body
	}
	if selStart < m.offset {
		m.offset = selStart
	}

	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[H") // begin synchronized update, cursor home
	b.WriteString(m.header(w, now, len(lines), body))
	b.WriteString("\x1b[K\r\n")
	for i := 0; i < body; i++ {
		if idx := m.offset + i; idx < len(lines) {
			b.WriteString(lines[idx])
		}
		b.WriteString("\x1b[K\r\n")
	}
	b.WriteString(m.statusBar(w, now))
	b.WriteString("\x1b[K\x1b[?2026l")
	return b.String()
}

func (m *model) header(w int, now time.Time, total, body int) string {
	clock := now.Format("15:04:05")
	l := newLine(w - 1)
	l.add(bold, " pipelines")
	l.add(grey, fmt.Sprintf(" · %s · group %s · %d projects", m.cfg.host, m.cfg.group, len(m.projects)))
	if m.hidePassed {
		l.add(yellow, "  [passed hidden]")
	}
	if !m.hideSkipped {
		l.add(yellow, "  [skipped shown]")
	}
	if total > body {
		l.add(grey, fmt.Sprintf("  ↕ %d-%d/%d", m.offset+1, min(m.offset+body, total), total))
	}
	l.padTo(w - 1 - width(clock))
	l.add(grey, clock)
	return l.String()
}

func (m *model) statusBar(w int, now time.Time) string {
	var right, rc string
	switch {
	case now.Before(m.flashUntil):
		right, rc = m.flash, cyan
	case m.fetching && (m.manual || !m.loaded || now.Sub(m.fetchStart) > 400*time.Millisecond):
		right, rc = spinner(now)+" refreshing", blue
	case m.lastErr != "":
		right, rc = "✕ "+m.lastErr+" · retry in "+ago(m.nextAt.Sub(now)), red
	case m.loaded:
		right, rc = "updated "+ago(now.Sub(m.lastFetch))+" ago · next "+ago(m.nextAt.Sub(now)), grey
		if now.Before(m.boostUntil) {
			right += " (fast)"
		}
	}
	hints := " ↑↓ select  ⏎ open  r refresh  f hide passed  s skipped  c compact  q quit"
	l := newLine(w - 1) // never touch the last column of the last row
	l.add(grey, truncate(hints, w-1-width(right)-2))
	l.padTo(w - 1 - width(right))
	l.add(rc, right)
	return l.String()
}

// ---------- terminal ----------

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func readKeys(ch chan<- string) {
	buf := make([]byte, 64)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return
		}
		for _, k := range parseKeys(buf[:n]) {
			ch <- k
		}
	}
}

func parseKeys(b []byte) []string {
	var out []string
	for i := 0; i < len(b); {
		if b[i] == 0x1b && i+2 < len(b) && (b[i+1] == '[' || b[i+1] == 'O') {
			j := i + 2
			for j < len(b) && (b[j] < 0x40 || b[j] > 0x7e) {
				j++
			}
			if j < len(b) {
				switch string(b[i+2 : j+1]) {
				case "A":
					out = append(out, "up")
				case "B":
					out = append(out, "down")
				case "H", "1~", "7~":
					out = append(out, "home")
				case "F", "4~", "8~":
					out = append(out, "end")
				case "5~":
					out = append(out, "pgup")
				case "6~":
					out = append(out, "pgdn")
				}
				i = j + 1
				continue
			}
		}
		out = append(out, string(b[i]))
		i++
	}
	return out
}

// ---------- main ----------

func main() {
	cfg := parseFlags()
	m := &model{cfg: cfg, c: newClient(cfg), entries: map[int]*entry{}, hideSkipped: cfg.hideSkipped, compact: cfg.compact}

	if cfg.once || !isTerminal(os.Stdout) || !isTerminal(os.Stdin) {
		r := fetch(m.c, cfg, nil, true, nil)
		if r.err != nil {
			fmt.Fprintln(os.Stderr, "pipeline:", r.err)
			os.Exit(1)
		}
		m.merge(r)
		w, _ := termSize()
		lines, _, _ := m.content(w, time.Now(), false)
		fmt.Println(strings.Join(lines, "\n"))
		return
	}
	os.Exit(run(m))
}

func run(m *model) int {
	restore, err := rawMode()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		return 1
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J") // alternate screen, hide cursor
	defer func() {
		os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
		restore()
	}()

	m.w, m.h = termSize()
	keys := make(chan string, 16)
	go readKeys(keys)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	results := make(chan fetchResult, 1)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()

	start := func(force bool) {
		if m.fetching {
			m.queued, m.queuedForce = true, m.queuedForce || force
			return
		}
		refreshProjects := force || m.projects == nil || time.Since(m.projectsAt) > projectsTTL
		m.fetching, m.fetchStart = true, time.Now()
		projects, known := m.projects, m.known()
		go func() { results <- fetch(m.c, m.cfg, projects, refreshProjects, known) }()
	}
	start(true)

	last := ""
	for {
		select {
		case k := <-keys:
			switch k {
			case "q", "Q", "\x03":
				return 0
			case "j", "down":
				m.move(1)
			case "k", "up":
				m.move(-1)
			case "g", "home":
				m.move(-1 << 20)
			case "G", "end":
				m.move(1 << 20)
			case "pgdn":
				m.move(5)
			case "pgup":
				m.move(-5)
			case "o", "\r", "\n":
				m.open()
			case "r":
				m.boostUntil, m.manual = time.Now().Add(boostFor), true
				start(true)
			case "f":
				m.hidePassed = !m.hidePassed
			case "s":
				m.hideSkipped = !m.hideSkipped
			case "c":
				m.compact = !m.compact
			}
		case r := <-results:
			m.fetching, m.manual = false, false
			m.merge(r)
			if m.queued {
				force := m.queuedForce
				m.queued, m.queuedForce = false, false
				m.manual = force
				start(force)
			} else {
				d := m.interval(time.Now())
				m.nextAt = time.Now().Add(d)
				timer.Reset(d)
			}
		case <-timer.C:
			start(false)
		case <-sigs:
			return 0
		case <-tick.C:
			// polled rather than SIGWINCH, which Windows doesn't have
			if w, h := termSize(); w != m.w || h != m.h {
				m.w, m.h = w, h
				os.Stdout.WriteString("\x1b[2J")
				last = ""
			}
		}
		if f := m.frame(time.Now()); f != last {
			os.Stdout.WriteString(f)
			last = f
		}
	}
}
