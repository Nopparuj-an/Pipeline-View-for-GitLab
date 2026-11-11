package app

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
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
func (m *model) renderCard(e *entry, selected, hovered bool, w int, now time.Time) []string {
	bc := grey
	if selected {
		bc = bold + cyan
	}
	if hovered {
		bc = bold + yellow
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

func (m *model) renderEntry(e *entry, selected, hovered bool, w int, now time.Time) []string {
	var lines []string
	start := func() *line {
		l := newLine(w)
		if hovered {
			l.add(yellow, "▌ ")
		} else if selected {
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
	if interactive {
		m.itemRows = nil
	}
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
		start := len(lines)
		if m.compact {
			lines = append(lines, m.renderEntry(e, sel, false, w, now)...)
		} else {
			lines = append(lines, m.renderCard(e, sel, false, w-1, now)...)
		}
		if sel {
			selEnd = len(lines)
		}
		if interactive {
			m.itemRows = append(m.itemRows, itemRow{start: start, end: len(lines), id: e.project.ID})
		}
	}
	if len(empty) > 0 {
		l := newLine(w)
		l.add(grey, "  no pipelines: "+strings.Join(empty, ", "))
		lines = append(lines, "", l.String())
	}
	return lines, selStart, selEnd
}

// frame renders the screen and returns the escape sequences that turn prev into it, rewriting
// only rows that changed: a full redraw ~10×/s (spinners) is enough output to make slower
// terminals such as VS Code's fall behind and block our writes, keys and Ctrl+C included.
func (m *model) frame(now time.Time, prev []string) (string, []string) {
	w, h := m.w, m.h
	body := max(0, h-2)
	lines, selStart, selEnd := m.content(w, now, true)
	m.totalLines = len(lines)

	// Scroll so the selected block is visible (its top wins if it is taller than the screen).
	m.offset = min(m.offset, max(0, len(lines)-body))
	if !m.freeScroll {
		if selEnd > m.offset+body {
			m.offset = selEnd - body
		}
		if selStart < m.offset {
			m.offset = selStart
		}
	}

	m.clicks = nil
	rows := make([]string, 0, body+2)
	rows = append(rows, m.header(w, now, len(lines), body))
	for i := 0; i < body; i++ {
		row := ""
		if idx := m.offset + i; idx < len(lines) {
			row = lines[idx]
		}
		rows = append(rows, row)
	}
	rows = append(rows, m.statusBar(w, now))
	if m.tagUI != nil && !m.tagUI.hidden {
		m.clicks = nil
		rows = m.tagRows(w, h)
	}
	if m.mouseX >= 1 && m.mouseX <= w && m.mouseY >= 1 && m.mouseY <= len(rows) {
		if m.tagUI == nil || m.tagUI.hidden {
			if m.mouseX < w && m.mouseY >= 2 && m.mouseY < h {
				row := m.offset + m.mouseY - 2
				for _, item := range m.itemRows {
					if row >= item.start && row < item.end {
						e := m.entries[item.id]
						var hovered []string
						if m.compact {
							hovered = m.renderEntry(e, item.id == m.selected, true, w, now)
						} else {
							hovered = m.renderCard(e, item.id == m.selected, true, w-1, now)
						}
						for i := max(0, item.start-m.offset); i < min(body, item.end-m.offset); i++ {
							rows[i+1] = hovered[m.offset+i-item.start]
						}
						break
					}
				}
			}
		}
		for _, target := range m.clicks {
			if m.mouseY == target.y && m.mouseX >= target.left && m.mouseX <= target.right {
				rows[target.y-1] = hoverRow(rows[target.y-1], target.left, target.right)
				break
			}
		}
	}
	m.screenRows = rows

	var b strings.Builder
	for i, row := range rows {
		if i < len(prev) && prev[i] == row {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("\x1b[?2026h") // begin synchronized update
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s\x1b[K", i+1, row)
	}
	if b.Len() > 0 {
		b.WriteString("\x1b[?2026l")
	}
	return b.String(), rows
}

// hoverRow adds a background to visible columns without disturbing text colours or OSC 8 links.
func hoverRow(row string, left, right int) string {
	const background = "\x1b[48;5;236m"
	var b strings.Builder
	col, active := 1, false
	for len(row) > 0 {
		if strings.HasPrefix(row, "\x1b[") {
			i := 2
			for i < len(row) && (row[i] < 0x40 || row[i] > 0x7e) {
				i++
			}
			if i == len(row) {
				break
			}
			seq := row[:i+1]
			b.WriteString(seq)
			if active && seq == reset {
				b.WriteString(background)
			}
			row = row[i+1:]
			continue
		}
		if strings.HasPrefix(row, "\x1b]8;;") {
			i := strings.Index(row[5:], "\x1b\\")
			if i < 0 {
				break
			}
			i += 7
			b.WriteString(row[:i])
			row = row[i:]
			continue
		}
		r, size := utf8.DecodeRuneInString(row)
		rw := runeWidth(r)
		if rw > 0 {
			within := col <= right && col+rw-1 >= left
			if within != active {
				if within {
					b.WriteString(background)
				} else {
					b.WriteString("\x1b[49m")
				}
				active = within
			}
			col += rw
		}
		b.WriteString(row[:size])
		row = row[size:]
	}
	if col <= right {
		if !active && col < left {
			padding := min(right+1, left) - col
			b.WriteString(strings.Repeat(" ", padding))
			col += padding
		}
		if col <= right {
			if !active {
				b.WriteString(background)
			}
			b.WriteString(strings.Repeat(" ", right-col+1))
			active = true
		}
	}
	if active {
		b.WriteString("\x1b[49m")
	}
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
	hints := " ↑↓ select  ⏎ open  t tag  r refresh  f hide passed  s skipped  c compact  q quit"
	l := newLine(w - 1) // never touch the last column of the last row
	visibleHints := truncate(hints, w-1-width(right)-2)
	l.add(grey, visibleHints)
	for _, action := range []struct{ text, key string }{
		{"↑↓ select", "down"}, {"⏎ open", "o"}, {"t tag", "t"}, {"r refresh", "r"},
		{"f hide passed", "f"}, {"s skipped", "s"}, {"c compact", "c"}, {"q quit", "q"},
	} {
		if i := strings.Index(visibleHints, action.text); i >= 0 {
			m.clicks = append(m.clicks, clickTarget{y: m.h, left: width(visibleHints[:i]) + 1, right: width(visibleHints[:i]) + width(action.text), key: action.key})
		}
	}
	l.padTo(w - 1 - width(right))
	l.add(rc, right)
	return l.String()
}
