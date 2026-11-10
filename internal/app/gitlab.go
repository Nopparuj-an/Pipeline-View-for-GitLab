package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

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
