// pipeline: live terminal view of the latest GitLab pipeline for each project in a group.
//
// Standard library only. Run straight from source:
//
//	go run . --group-id=my-group --host=gitlab.example.com --include="web-*,api-*"
//
// Keys: ↑/↓ or j/k select, Enter/o open pipeline, r refresh now (and poll fast for a while),
// f hide passed pipelines, s toggle skipped jobs, c compact layout, q quit.
package app

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pipeline/internal/terminal"
)

const (
	projectsTTL = 5 * time.Minute // how often the project list itself is re-fetched
	boostFor    = 2 * time.Minute // fast polling after pressing r (e.g. right after a push)
	newBadgeFor = time.Minute     // how long a freshly appeared pipeline is marked NEW
	durW        = 6               // width of job durations like "12m03s"
)

func Main() {
	cfg := parseFlags()
	m := &model{cfg: cfg, c: newClient(cfg), entries: map[int]*entry{}, hideSkipped: cfg.hideSkipped, compact: cfg.compact}

	if cfg.once || !terminal.IsTerminal(os.Stdout) || !terminal.IsTerminal(os.Stdin) {
		r := fetch(m.c, cfg, nil, true, nil)
		if r.err != nil {
			fmt.Fprintln(os.Stderr, "pipeline:", r.err)
			os.Exit(1)
		}
		m.merge(r)
		w, _ := terminal.Size()
		lines, _, _ := m.content(w, time.Now(), false)
		fmt.Println(strings.Join(lines, "\n"))
		return
	}
	os.Exit(run(m))
}

func run(m *model) int {
	restore, err := terminal.RawMode()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pipeline:", err)
		return 1
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l\x1b[2J") // alternate screen, hide cursor
	defer func() {
		os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
		restore()
	}()

	m.w, m.h = terminal.Size()
	keys := make(chan string, 16)
	go terminal.ReadKeys(keys)
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

	var drawn []string // rows currently on screen
	for {
		select {
		case k := <-keys:
			if m.tagUI != nil && !m.tagUI.hidden {
				if k == "\x03" {
					return 0
				}
				m.tagKey(k)
				break
			}
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
			case "t":
				m.openTagger()
			}
		case event := <-m.tagEvents:
			m.handleTagEvent(event)
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
			if w, h := terminal.Size(); w != m.w || h != m.h {
				m.w, m.h = w, h
				os.Stdout.WriteString("\x1b[2J")
				drawn = nil
			}
		}
		var f string
		if f, drawn = m.frame(time.Now(), drawn); f != "" {
			os.Stdout.WriteString(f)
		}
	}
}
