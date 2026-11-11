package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"pipeline/internal/terminal"
)

func mouseModel(compact bool) *model {
	m := &model{cfg: &config{}, loaded: true, entries: map[int]*entry{}, w: 100, h: 8, compact: compact}
	for i := 1; i <= 8; i++ {
		p := Project{ID: i, Name: fmt.Sprintf("project-%d", i), WebURL: "https://example.com/project"}
		m.projects = append(m.projects, p)
		m.entries[i] = &entry{project: p, pipeline: &Pipeline{ID: i, WebURL: "https://example.com/pipeline", Status: "failed", UpdatedAt: time.Unix(int64(100-i), 0)}}
	}
	return m
}

func TestMouseScrollAndSelection(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			m := mouseModel(compact)
			now := time.Now()
			m.frame(now, nil)
			m.mouse(terminal.Mouse{Button: 65, X: 10, Y: 3}, now)
			m.frame(now, nil)
			if m.offset != 3 || m.selected != 1 {
				t.Fatalf("scroll offset=%d selected=%d", m.offset, m.selected)
			}
			m.frame(now.Add(time.Second), nil)
			if m.offset != 3 {
				t.Fatal("redraw followed selection")
			}
			item := m.itemRows[2]
			y := item.start - m.offset + 2
			if y < 2 || y >= m.h {
				t.Fatalf("item row offscreen: %d", y)
			}
			click := terminal.Mouse{X: 3, Y: y}
			if k := m.mouse(click, now); k != "" || m.selected != item.id {
				t.Fatalf("single click: key=%q selected=%d", k, m.selected)
			}
			if k := m.mouse(click, now.Add(200*time.Millisecond)); k != "t" {
				t.Fatalf("double click: %q", k)
			}
			m.move(-1)
			m.frame(now, nil)
			if m.freeScroll || m.offset > m.itemRows[1].start {
				t.Fatal("keyboard did not follow selection")
			}
		})
	}
}

func TestClickTargets(t *testing.T) {
	m := mouseModel(false)
	now := time.Now()
	m.frame(now, nil)
	var refresh clickTarget
	for _, target := range m.clicks {
		if target.key == "r" {
			refresh = target
		}
	}
	if refresh.key == "" || m.mouse(terminal.Mouse{X: refresh.left, Y: refresh.y}, now) != "r" {
		t.Fatal("refresh hint not clickable")
	}
	m.tagUI = &tagUI{phase: "menu", inventory: tagInventory{tags: map[string]tag{}}}
	m.frame(now, nil)
	for _, target := range m.clicks {
		if target.key == "menu:3" {
			key := m.mouse(terminal.Mouse{X: target.left, Y: target.y}, now)
			m.tagKey(key)
			if m.tagUI.phase != "manual-kind" {
				t.Fatal("manual menu item not activated")
			}
			m.frame(now, nil)
			for _, kind := range m.clicks {
				if kind.key == "menu:1" {
					m.tagKey(m.mouse(terminal.Mouse{X: kind.left, Y: kind.y}, now))
					if m.tagUI.phase != "manual-input" || m.tagUI.manualKind != "prod" {
						t.Fatal("manual kind not activated")
					}
					return
				}
			}
			t.Fatal("missing prod click target")
		}
	}
	t.Fatal("missing manual click target")
}

func TestLinkAt(t *testing.T) {
	l := newLine(30)
	l.add(grey, "界 ")
	l.link(bold, "https://example.com", "link")
	l.add("", " end")
	for col := 1; col <= 11; col++ {
		got := linkAt(l.String(), col)
		if (got != "") != (col >= 4 && col <= 7) || (got != "" && !strings.HasPrefix(got, "https://")) {
			t.Fatalf("column %d: %q", col, got)
		}
	}
}

func TestMouseBoundsAndClippedActions(t *testing.T) {
	m := mouseModel(false)
	now := time.Now()
	m.w, m.h = 30, 6
	m.frame(now, nil)
	for _, target := range m.clicks {
		if target.left < 1 || target.right >= m.w || target.y > m.h {
			t.Fatalf("out-of-bounds target: %+v", target)
		}
	}
	for _, e := range []terminal.Mouse{
		{X: 1, Y: 0}, {X: 1, Y: 7}, {X: 31, Y: 2},
		{Button: 2, X: 1, Y: 2}, {X: 1, Y: 2, Release: true},
	} {
		m.mouse(e, now)
		if m.selected != 1 || !m.lastClickAt.IsZero() {
			t.Fatalf("unexpected click: %+v", e)
		}
	}
	m.offset = 1000
	m.freeScroll = true
	m.frame(now, nil)
	if m.offset != max(0, m.totalLines-(m.h-2)) {
		t.Fatalf("offset was not clamped after resize: %d", m.offset)
	}
	m.tagUI = &tagUI{phase: "confirm"}
	m.w, m.h = 20, 5
	m.frame(now, nil)
	for _, target := range m.clicks {
		if target.y > m.h || target.right >= m.w {
			t.Fatalf("clipped menu action is clickable: %+v", target)
		}
	}
}

func TestDoubleClickInterrupted(t *testing.T) {
	m := mouseModel(false)
	now := time.Now()
	m.frame(now, nil)
	click := terminal.Mouse{X: 3, Y: 2}
	m.mouse(click, now)
	m.mouse(terminal.Mouse{Button: 65, X: 3, Y: 2}, now)
	m.frame(now, nil)
	if k := m.mouse(click, now.Add(100*time.Millisecond)); k != "" {
		t.Fatalf("scroll should interrupt double click, got %q", k)
	}
}

func TestMouseHover(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprint(compact), func(t *testing.T) {
			m := mouseModel(compact)
			now := time.Now()
			_, before := m.frame(now, nil)
			item := m.itemRows[1]
			y := item.start - m.offset + 2
			m.mouse(terminal.Mouse{Button: 35, X: 3, Y: y}, now)
			_, rows := m.frame(now, before)
			if m.selected != 1 || !strings.Contains(rows[y-1], yellow) || strings.Contains(rows[y-1], "\x1b[48;5;236m") {
				t.Fatalf("hover changed selection or did not color the edge: selected=%d row=%q", m.selected, rows[y-1])
			}
			for col := 1; col < m.w; col++ {
				if got, want := linkAt(rows[y-1], col), linkAt(before[y-1], col); got != want {
					t.Fatalf("hover changed link at column %d: got %q, want %q", col, got, want)
				}
			}
			m.mouse(terminal.Mouse{Button: 35, X: 1, Y: 1}, now)
			_, rows = m.frame(now, rows)
			if rows[y-1] != before[y-1] {
				t.Fatal("highlight did not clear")
			}
		})
	}
}

func TestHoverStatusAction(t *testing.T) {
	m := mouseModel(false)
	now := time.Now()
	m.frame(now, nil)
	for _, target := range m.clicks {
		if target.key == "r" {
			m.mouse(terminal.Mouse{Button: 35, X: target.left, Y: target.y}, now)
			_, rows := m.frame(now, nil)
			if !strings.Contains(rows[target.y-1], "\x1b[48;5;236m") {
				t.Fatal("status action hover missing")
			}
			if key := m.mouse(terminal.Mouse{X: target.left, Y: target.y}, now); key != "r" {
				t.Fatalf("hovered action returned %q", key)
			}
			return
		}
	}
	t.Fatal("missing refresh target")
}

func TestHoverFollowsScrollAndActions(t *testing.T) {
	m := mouseModel(false)
	now := time.Now()
	m.frame(now, nil)
	m.mouse(terminal.Mouse{Button: 35, X: 3, Y: 2}, now)
	m.mouse(terminal.Mouse{Button: 65, X: 3, Y: 2}, now)
	_, rows := m.frame(now, nil)
	if m.offset != 3 || !strings.Contains(rows[1], yellow) || strings.Contains(rows[1], "\x1b[48;5;236m") || m.selected != 1 {
		t.Fatal("hover did not follow viewport after scroll")
	}
	m.tagUI = &tagUI{phase: "menu"}
	m.frame(now, nil)
	for _, target := range m.clicks {
		if target.key == "menu:3" {
			m.mouse(terminal.Mouse{Button: 35, X: target.left, Y: target.y}, now)
			_, rows = m.frame(now, nil)
			if !strings.Contains(rows[target.y-1], "\x1b[48;5;236m") || m.tagUI.selected != 0 {
				t.Fatal("menu hover missing or changed selection")
			}
			return
		}
	}
	t.Fatal("missing menu target")
}

func TestMenuEmptySpaceHoverAndClick(t *testing.T) {
	for _, phase := range []string{"menu", "manual-kind"} {
		t.Run(phase, func(t *testing.T) {
			m := mouseModel(false)
			m.tagUI = &tagUI{phase: phase}
			now := time.Now()
			m.frame(now, nil)
			for _, target := range m.clicks {
				if target.key != "menu:1" {
					continue
				}
				if target.left != 3 || target.right != m.w-1 {
					t.Fatalf("menu width: %+v", target)
				}
				m.mouse(terminal.Mouse{Button: 35, X: target.right, Y: target.y}, now)
				_, rows := m.frame(now, nil)
				if !strings.Contains(rows[target.y-1], "\x1b[48;5;236m") {
					t.Fatal("empty space did not highlight")
				}
				if key := m.mouse(terminal.Mouse{X: target.right, Y: target.y}, now); key != "\r" || m.tagUI.selected != 1 {
					t.Fatalf("empty space click: key=%q selected=%d", key, m.tagUI.selected)
				}
				return
			}
			t.Fatal("missing menu target")
		})
	}
}
