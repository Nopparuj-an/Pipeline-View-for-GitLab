package app

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"pipeline/internal/terminal"
)

type itemRow struct{ start, end, id int }
type clickTarget struct {
	y, left, right int
	key            string
}

func (m *model) mouse(e terminal.Mouse, now time.Time) string {
	m.mouseX, m.mouseY = e.X, e.Y
	if e.X < 1 || e.X > m.w || e.Y < 1 || e.Y > m.h || e.Release {
		return ""
	}
	if e.Button == 64 || e.Button == 65 {
		m.lastClickAt = time.Time{}
		if m.tagUI == nil || m.tagUI.hidden {
			delta := -3
			if e.Button == 65 {
				delta = 3
			}
			m.offset = max(0, min(max(0, m.totalLines-max(0, m.h-2)), m.offset+delta))
			m.freeScroll = true
		}
		return ""
	}
	// Ignore releases, motion, right/middle clicks and modified clicks except
	// Ctrl/Alt on a hyperlink. Command-click is usually handled by the terminal.
	if e.Button&3 != 0 || e.Button&32 != 0 {
		return ""
	}
	if e.Button&(8|16) != 0 {
		m.lastClickAt = time.Time{}
		if e.Y <= len(m.screenRows) {
			if href := linkAt(m.screenRows[e.Y-1], e.X); href != "" {
				m.openURL(href, "link")
			}
		}
		return ""
	}
	if e.Button != 0 {
		return ""
	}
	for _, target := range m.clicks {
		if e.Y == target.y && e.X >= target.left && e.X <= target.right {
			m.lastClickAt = time.Time{}
			if strings.HasPrefix(target.key, "menu:") && m.tagUI != nil && !m.tagUI.hidden {
				i, err := strconv.Atoi(strings.TrimPrefix(target.key, "menu:"))
				if err == nil {
					m.tagUI.selected = i
					return "\r"
				}
			}
			return target.key
		}
	}
	if (m.tagUI != nil && !m.tagUI.hidden) || e.Y < 2 || e.Y >= m.h {
		m.lastClickAt = time.Time{}
		return ""
	}
	row := m.offset + e.Y - 2
	for _, item := range m.itemRows {
		if row >= item.start && row < item.end {
			m.selected = item.id
			m.freeScroll = true
			if m.lastClickID == item.id && !m.lastClickAt.IsZero() && now.Sub(m.lastClickAt) >= 0 && now.Sub(m.lastClickAt) <= 500*time.Millisecond {
				m.lastClickAt = time.Time{}
				return "t"
			}
			m.lastClickID, m.lastClickAt = item.id, now
			return ""
		}
	}
	m.lastClickAt = time.Time{}
	return ""
}

// linkAt locates an OSC 8 hyperlink by its visible terminal column.
func linkAt(row string, column int) string {
	col, href := 1, ""
	for len(row) > 0 {
		if strings.HasPrefix(row, "\x1b]8;;") {
			end := strings.Index(row[5:], "\x1b\\")
			if end < 0 {
				return ""
			}
			href = row[5 : 5+end]
			row = row[5+end+2:]
			continue
		}
		if strings.HasPrefix(row, "\x1b[") {
			i := 2
			for i < len(row) && (row[i] < 0x40 || row[i] > 0x7e) {
				i++
			}
			if i == len(row) {
				return ""
			}
			row = row[i+1:]
			continue
		}
		r, size := utf8.DecodeRuneInString(row)
		w := runeWidth(r)
		if column >= col && column < col+w {
			return href
		}
		col += w
		row = row[size:]
	}
	return ""
}
