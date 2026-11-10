package app

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
