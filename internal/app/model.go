package app

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"
)

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
	tagUI       *tagUI
	tagCancel   context.CancelFunc
	tagEvents   chan tagEvent
	tagSequence int
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
