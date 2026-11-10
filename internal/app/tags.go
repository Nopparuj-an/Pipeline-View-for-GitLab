package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type version struct{ major, minor, patch int }

var versionPattern = regexp.MustCompile(`^(?:uat-)?v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func parseVersion(s string) (version, bool) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return version{}, false
	}
	a, e1 := strconv.Atoi(m[1])
	b, e2 := strconv.Atoi(m[2])
	c, e3 := strconv.Atoi(m[3])
	return version{a, b, c}, e1 == nil && e2 == nil && e3 == nil
}
func (v version) String() string { return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch) }
func (v version) less(w version) bool {
	if v.major != w.major {
		return v.major < w.major
	}
	if v.minor != w.minor {
		return v.minor < w.minor
	}
	return v.patch < w.patch
}

type tag struct {
	Name      string     `json:"name"`
	CreatedAt *time.Time `json:"created_at"`
	Commit    struct {
		ID string `json:"id"`
	} `json:"commit"`
}

type tagInventory struct {
	tags      map[string]tag
	uat, prod *version
}

func (i tagInventory) next() version {
	var v version
	if i.uat != nil {
		v = *i.uat
	}
	if i.prod != nil && v.less(*i.prod) {
		v = *i.prod
	}
	v.patch++
	return v
}
func (i tagInventory) candidate() *version {
	if i.uat != nil {
		if _, ok := i.tags[i.uat.String()]; !ok {
			v := *i.uat
			return &v
		}
	}
	return nil
}

func (c *client) tags(ctx context.Context, project int) (tagInventory, error) {
	i := tagInventory{tags: map[string]tag{}}
	for page := "1"; page != ""; {
		var tags []tag
		next, err := c.get(ctx, fmt.Sprintf("/projects/%d/repository/tags?per_page=100&page=%s", project, url.QueryEscape(page)), &tags)
		if err != nil {
			return i, err
		}
		for _, t := range tags {
			i.tags[t.Name] = t
			v, ok := parseVersion(t.Name)
			if !ok {
				continue
			}
			if strings.HasPrefix(t.Name, "uat-") {
				if i.uat == nil || i.uat.less(v) {
					copy := v
					i.uat = &copy
				}
			} else if i.prod == nil || i.prod.less(v) {
				copy := v
				i.prod = &copy
			}
		}
		page = next
	}
	return i, nil
}

func (c *client) mainSHA(ctx context.Context, project int) (string, error) {
	var branch struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	_, err := c.get(ctx, fmt.Sprintf("/projects/%d/repository/branches/main", project), &branch)
	if err == nil && branch.Commit.ID == "" {
		err = fmt.Errorf("main branch has no commit")
	}
	return branch.Commit.ID, err
}

func (c *client) createTag(ctx context.Context, project int, name, ref string) (tag, error) {
	var t tag
	form := url.Values{"tag_name": {name}, "ref": {ref}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/projects/%d/repository/tags", c.base, project), strings.NewReader(form.Encode()))
	if err != nil {
		return t, err
	}
	req.Header.Set("Private-Token", c.token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return t, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return t, err
	}
	if resp.StatusCode != http.StatusCreated {
		return t, fmt.Errorf("creating %s: HTTP %d: %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return t, err
	}
	if t.Commit.ID == "" {
		return t, fmt.Errorf("created %s but response has no commit ID", name)
	}
	return t, nil
}

// waitUAT requires a tag pipeline for this exact commit, not a branch pipeline with the same ref.
func (c *client) waitUAT(ctx context.Context, project int, name, sha string, createdAfter time.Time, progress func(string), interval, missingTimeout time.Duration) error {
	deadline := time.Now().Add(missingTimeout)
	for {
		var pipelines []struct {
			ID       int `json:"id"`
			SHA, Ref string
		}
		path := fmt.Sprintf("/projects/%d/pipelines?ref=%s&sha=%s&per_page=100&order_by=id&sort=desc", project, url.QueryEscape(name), url.QueryEscape(sha))
		if !createdAfter.IsZero() {
			path += "&created_after=" + url.QueryEscape(createdAfter.UTC().Format(time.RFC3339Nano))
		}
		if _, err := c.get(ctx, path, &pipelines); err != nil {
			return err
		}
		found := false
		for _, p := range pipelines {
			if p.Ref != name || p.SHA != sha {
				continue
			}
			var detail struct {
				ID               int `json:"id"`
				Status, SHA, Ref string
				Tag              bool `json:"tag"`
			}
			if _, err := c.get(ctx, fmt.Sprintf("/projects/%d/pipelines/%d", project, p.ID), &detail); err != nil {
				return err
			}
			if !detail.Tag || detail.Ref != name || detail.SHA != sha {
				continue
			}
			found = true
			progress(fmt.Sprintf("%s pipeline #%d: %s", name, p.ID, detail.Status))
			switch detail.Status {
			case "success":
				return nil
			case "created", "waiting_for_resource", "waiting_for_callback", "preparing", "pending", "running", "scheduled":
			default:
				return fmt.Errorf("%s pipeline #%d is %s", name, p.ID, detail.Status)
			}
			break
		}
		if !found && time.Now().After(deadline) {
			return fmt.Errorf("no tag pipeline appeared for %s", name)
		}
		if !found {
			progress("Waiting for pipeline for " + name)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
