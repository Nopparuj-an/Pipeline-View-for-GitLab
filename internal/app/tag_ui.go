package app

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type tagUI struct {
	project    Project
	phase      string
	selected   int
	manualKind string
	input      string
	inventory  tagInventory
	plan       tagPlan
	message    string
	id         int
	hidden     bool
}

type tagPlan struct {
	mode, name, sha string
	createUAT       bool
}

type tagEvent struct {
	id        int
	kind      string
	inventory tagInventory
	plan      tagPlan
	message   string
	err       error
}

func (m *model) openTagger() {
	if m.tagUI != nil {
		m.tagUI.hidden = false
		return
	}
	shown, _ := m.visible()
	if len(shown) == 0 {
		return
	}
	e := shown[m.selectedIndex(shown)]
	if e.pipeline == nil {
		m.setFlash("project has no pipeline to tag")
		return
	}
	if m.tagEvents == nil {
		m.tagEvents = make(chan tagEvent, 32)
	}
	m.tagSequence++
	m.tagUI = &tagUI{project: e.project, phase: "loading", id: m.tagSequence, message: "Loading tags…"}
	ctx, cancel := context.WithCancel(context.Background())
	m.tagCancel = cancel
	id, project := m.tagUI.id, e.project.ID
	go func() {
		inv, err := m.c.tags(ctx, project)
		m.sendTagEvent(ctx, tagEvent{id: id, kind: "loaded", inventory: inv, err: err})
	}()
}

func (m *model) sendTagEvent(ctx context.Context, e tagEvent) {
	select {
	case m.tagEvents <- e:
	case <-ctx.Done():
	}
}

func (m *model) closeTagger() {
	if m.tagCancel != nil {
		m.tagCancel()
		m.tagCancel = nil
	}
	if m.tagUI != nil && m.tagUI.phase == "working" {
		m.setFlash("Tag operation stopped; check GitLab for any created tags")
	}
	m.tagUI = nil
}

func (m *model) tagKey(k string) {
	u := m.tagUI
	if k == "\x1b" {
		m.closeTagger()
		return
	}
	switch u.phase {
	case "menu":
		switch k {
		case "up", "k":
			u.selected = (u.selected + 3) % 4
		case "down", "j":
			u.selected = (u.selected + 1) % 4
		case "\r", "\n":
			mode := []string{"auto", "uat", "prod", "manual"}[u.selected]
			if mode == "manual" {
				u.phase, u.selected = "manual-kind", 0
				return
			}
			m.prepareTag(mode, "")
		}
	case "manual-kind":
		switch k {
		case "up", "down", "j", "k":
			u.selected = 1 - u.selected
		case "\r", "\n":
			u.manualKind = []string{"uat", "prod"}[u.selected]
			v := u.inventory.next()
			if u.manualKind == "prod" && u.inventory.candidate() != nil {
				v = *u.inventory.candidate()
			}
			u.input, u.phase = v.String(), "manual-input"
		}
	case "manual-input":
		switch k {
		case "\r", "\n":
			m.prepareTag(u.manualKind, u.input)
		case "\x7f", "\b":
			if len(u.input) > 0 {
				u.input = u.input[:len(u.input)-1]
			}
		case "\x15":
			u.input = ""
		default:
			if len(k) == 1 && ((k[0] >= '0' && k[0] <= '9') || k == "." || k == "v") && len(u.input) < 50 {
				u.input += k
			}
		}
	case "confirm":
		switch k {
		case "\r", "\n", "y", "Y":
			m.executeTag()
		case "n", "N":
			u.phase = "menu"
		}
	case "working":
		if k == "b" {
			u.hidden = true
			m.setFlash("Tagging in background; press t to view")
		}
	case "done", "error":
		if k == "\r" || k == "\n" {
			m.closeTagger()
		}
	}
}

func (m *model) prepareTag(mode, input string) {
	u := m.tagUI
	v := u.inventory.next()
	if input != "" {
		var ok bool
		v, ok = parseVersion(input)
		if !ok || strings.HasPrefix(input, "uat-") {
			u.message = "Enter a version like v1.2.3"
			return
		}
	} else if u.phase == "manual-input" {
		u.message = "Enter a version like v1.2.3"
		return
	} else if (mode == "auto" || mode == "prod") && u.inventory.candidate() != nil {
		v = *u.inventory.candidate()
	} else if mode == "prod" {
		u.message = "No unpromoted UAT version available"
		return
	}
	name := v.String()
	plan := tagPlan{mode: mode, name: name}
	uat, exists := u.inventory.tags["uat-"+name]
	_, prodExists := u.inventory.tags[name]
	if mode == "uat" || (mode == "auto" && !exists) {
		if exists {
			u.message = "UAT tag already exists: uat-" + name
			return
		}
		if prodExists {
			u.message = "Production tag already exists: " + name
			return
		}
		plan.createUAT = true
	} else {
		if !exists {
			u.message = "Matching UAT tag does not exist: uat-" + name
			return
		}
		if prodExists {
			u.message = "Production tag already exists: " + name
			return
		}
		if uat.Commit.ID == "" {
			u.message = "UAT tag has no commit ID"
			return
		}
		plan.sha = uat.Commit.ID
	}
	u.phase, u.message = "preparing", "Preparing tag…"
	ctx, cancel := context.WithCancel(context.Background())
	if m.tagCancel != nil {
		m.tagCancel()
	}
	m.tagCancel = cancel
	id, project := u.id, u.project.ID
	go func() {
		var err error
		if plan.createUAT {
			plan.sha, err = m.c.mainSHA(ctx, project)
		}
		m.sendTagEvent(ctx, tagEvent{id: id, kind: "prepared", plan: plan, err: err})
	}()
}

func (m *model) executeTag() {
	u := m.tagUI
	u.phase, u.message = "working", "Checking tags…"
	ctx, cancel := context.WithCancel(context.Background())
	if m.tagCancel != nil {
		m.tagCancel()
	}
	m.tagCancel = cancel
	id, project, plan := u.id, u.project.ID, u.plan
	go func() {
		progress := func(s string) { m.sendTagEvent(ctx, tagEvent{id: id, kind: "progress", message: s}) }
		err := m.runTag(ctx, project, plan, progress)
		m.sendTagEvent(ctx, tagEvent{id: id, kind: "finished", err: err})
	}()
}

func (m *model) runTag(ctx context.Context, project int, plan tagPlan, progress func(string)) error {
	inv, err := m.c.tags(ctx, project)
	if err != nil {
		return err
	}
	uatName := "uat-" + plan.name
	var createdAfter time.Time
	if plan.createUAT {
		if _, ok := inv.tags[uatName]; ok {
			return fmt.Errorf("tag already exists: %s", uatName)
		}
		if _, ok := inv.tags[plan.name]; ok {
			return fmt.Errorf("tag already exists: %s", plan.name)
		}
		progress("Creating " + uatName)
		createdAfter = time.Now().Add(-time.Second)
		t, err := m.c.createTag(ctx, project, uatName, plan.sha)
		if err != nil {
			return err
		}
		if t.Commit.ID != plan.sha {
			return fmt.Errorf("%s created on unexpected commit; not promoting", uatName)
		}
		if plan.mode == "uat" {
			return nil
		}
	} else {
		t, ok := inv.tags[uatName]
		if !ok || t.Commit.ID != plan.sha {
			return fmt.Errorf("%s changed or was deleted; not promoting", uatName)
		}
		if t.CreatedAt != nil {
			createdAfter = t.CreatedAt.Add(-time.Second)
		}
	}
	if err := m.c.waitUAT(ctx, project, uatName, plan.sha, createdAfter, progress, 2*time.Second, time.Minute); err != nil {
		return err
	}
	// Recheck both tags after waiting, because another operator may have changed them.
	inv, err = m.c.tags(ctx, project)
	if err != nil {
		return err
	}
	t, ok := inv.tags[uatName]
	if !ok || t.Commit.ID != plan.sha {
		return fmt.Errorf("%s changed or was deleted; not promoting", uatName)
	}
	if _, ok := inv.tags[plan.name]; ok {
		return fmt.Errorf("tag already exists: %s", plan.name)
	}
	progress("Creating " + plan.name)
	_, err = m.c.createTag(ctx, project, plan.name, plan.sha)
	return err
}

func (m *model) handleTagEvent(e tagEvent) {
	u := m.tagUI
	if u == nil || u.id != e.id {
		return
	}
	if e.err != nil {
		u.phase, u.message = "error", e.err.Error()
		if u.hidden {
			m.setFlash("Tagging failed; press t for details")
		}
		return
	}
	switch e.kind {
	case "loaded":
		u.inventory, u.phase, u.message = e.inventory, "menu", ""
	case "prepared":
		u.plan, u.phase, u.message = e.plan, "confirm", ""
	case "progress":
		u.message = e.message
	case "finished":
		u.phase = "done"
		if u.plan.mode == "uat" {
			u.message = "Created uat-" + u.plan.name
		} else {
			u.message = "Created " + u.plan.name
		}
		if u.hidden {
			m.setFlash(u.message + " · press t for details")
		}
	}
}

func (m *model) tagRows(w, h int) []string {
	u := m.tagUI
	var lines []string
	add := func(style, s string) {
		l := newLine(max(1, w-1))
		l.add(style, "  "+s)
		lines = append(lines, l.String())
	}
	action := func(s, text, key string) {
		if i := strings.Index(s, text); i >= 0 && 2+width(s[:i])+width(text) <= max(1, w-1) && len(lines) <= h {
			left := 3 + width(s[:i])
			m.clicks = append(m.clicks, clickTarget{y: len(lines), left: left, right: left + width(text) - 1, key: key})
		}
	}
	add(bold, "Tag project: "+u.project.Name)
	lines = append(lines, "")
	if u.phase != "loading" {
		latest := func(v *version, prefix string) string {
			if v == nil {
				return "none"
			}
			return prefix + v.String()
		}
		add(grey, "Latest UAT: "+latest(u.inventory.uat, "uat-")+"    Prod: "+latest(u.inventory.prod, ""))
		lines = append(lines, "")
	}
	switch u.phase {
	case "menu":
		for i, item := range []string{"Auto", "UAT", "Prod", "Manual"} {
			if i == u.selected {
				add(cyan, "› "+item)
			} else {
				add(grey, "  "+item)
			}
			if len(lines) <= h && w > 3 {
				m.clicks = append(m.clicks, clickTarget{y: len(lines), left: 3, right: w - 1, key: fmt.Sprintf("menu:%d", i)})
			}
		}
		hint := "↑/↓ select · Enter continue · Esc leave"
		add(grey, hint)
		action(hint, "Enter continue", "\r")
		action(hint, "Esc leave", "\x1b")
	case "manual-kind":
		add(bold, "Choose tag type:")
		for i, item := range []string{"UAT", "Prod"} {
			if i == u.selected {
				add(cyan, "› "+item)
			} else {
				add(grey, "  "+item)
			}
			if len(lines) <= h && w > 3 {
				m.clicks = append(m.clicks, clickTarget{y: len(lines), left: 3, right: w - 1, key: fmt.Sprintf("menu:%d", i)})
			}
		}
		hint := "↑/↓ select · Enter continue · Esc leave"
		add(grey, hint)
		action(hint, "Enter continue", "\r")
		action(hint, "Esc leave", "\x1b")
	case "manual-input":
		add(bold, "Version for "+strings.ToUpper(u.manualKind)+": "+u.input+"_")
		hint := "Backspace edit · Ctrl+U clear · Enter continue · Esc leave"
		add(grey, hint)
		action(hint, "Backspace edit", "\x7f")
		action(hint, "Ctrl+U clear", "\x15")
		action(hint, "Enter continue", "\r")
		action(hint, "Esc leave", "\x1b")
	case "confirm":
		if u.plan.createUAT {
			add(bold, "Create uat-"+u.plan.name+" from main commit:")
		} else {
			add(bold, "Promote uat-"+u.plan.name+" to "+u.plan.name+" from commit:")
		}
		add(cyan, u.plan.sha)
		if u.plan.mode == "auto" && u.plan.createUAT {
			add(grey, "Then promote to "+u.plan.name+" after the UAT pipeline succeeds.")
		}
		hint := "Enter/Y confirm · N back · Esc leave"
		add(grey, hint)
		action(hint, "Enter/Y confirm", "\r")
		action(hint, "N back", "n")
		action(hint, "Esc leave", "\x1b")
	case "working":
		hint := "B background · Esc stops waiting; created tags remain."
		add(grey, hint)
		action(hint, "B background", "b")
		action(hint, "Esc stops waiting", "\x1b")
	case "error", "done":
		hint := "Enter/Esc leave"
		add(grey, hint)
		action(hint, hint, "\r")
	}
	if u.message != "" {
		lines = append(lines, "")
		add(yellow, u.message)
	}
	rows := make([]string, max(1, h))
	copy(rows, lines)
	return rows
}
