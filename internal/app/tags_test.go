package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTagsPaginationAndVersions(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"name":"uat-v1.9.9"},{"name":"v1.10.0"},{"name":"v01.0.0"}]`)
		} else {
			fmt.Fprint(w, `[{"name":"uat-v1.10.1"},{"name":"unrelated"}]`)
		}
	}))
	defer s.Close()
	c := newClient(&config{host: s.URL})
	i, err := c.tags(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if i.uat.String() != "v1.10.1" || i.prod.String() != "v1.10.0" || i.next().String() != "v1.10.2" || i.candidate().String() != "v1.10.1" {
		t.Fatalf("unexpected inventory: %+v", i)
	}
	if (tagInventory{}).next().String() != "v0.0.1" {
		t.Fatal("wrong starting version")
	}
}

func TestWaitUAT(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		tag          bool
		wantErr      bool
	}{
		{"success", "success", true, false}, {"failed", "failed", true, true}, {"manual", "manual", true, true}, {"branch", "success", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/pipelines") {
					if r.URL.Query().Get("ref") != "uat-v1.0.0" {
						t.Error("missing ref")
					}
					fmt.Fprint(w, `[{"id":12,"ref":"uat-v1.0.0","sha":"abc"}]`)
				} else {
					fmt.Fprintf(w, `{"id":12,"ref":"uat-v1.0.0","sha":"abc","tag":%t,"status":%q}`, tc.tag, tc.status)
				}
			}))
			defer s.Close()
			c := newClient(&config{host: s.URL})
			err := c.waitUAT(context.Background(), 1, "uat-v1.0.0", "abc", time.Time{}, func(string) {}, time.Millisecond, 2*time.Millisecond)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestAutoCreatesUATThenPromotesAfterSuccess(t *testing.T) {
	tags := map[string]string{}
	statusCalls := 0
	var created []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/repository/tags") && r.Method == http.MethodGet:
			var result []map[string]any
			for name, sha := range tags {
				result = append(result, map[string]any{"name": name, "commit": map[string]string{"id": sha}})
			}
			json.NewEncoder(w).Encode(result)
		case strings.HasSuffix(r.URL.Path, "/repository/tags") && r.Method == http.MethodPost:
			r.ParseForm()
			name, sha := r.Form.Get("tag_name"), r.Form.Get("ref")
			if name == "v0.0.1" && statusCalls < 2 {
				t.Error("promoted before success")
			}
			tags[name] = sha
			created = append(created, name)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"name": name, "commit": map[string]string{"id": sha}})
		case strings.HasSuffix(r.URL.Path, "/pipelines"):
			fmt.Fprint(w, `[{"id":7,"ref":"uat-v0.0.1","sha":"abc"}]`)
		case strings.HasSuffix(r.URL.Path, "/pipelines/7"):
			statusCalls++
			status := "running"
			if statusCalls >= 2 {
				status = "success"
			}
			fmt.Fprintf(w, `{"id":7,"ref":"uat-v0.0.1","sha":"abc","tag":true,"status":%q}`, status)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	m := &model{c: newClient(&config{host: s.URL})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := m.runTag(ctx, 1, tagPlan{mode: "auto", name: "v0.0.1", sha: "abc", createUAT: true}, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(created, ",") != "uat-v0.0.1,v0.0.1" {
		t.Fatalf("created %v", created)
	}
}

func TestTagMenuAndManualInput(t *testing.T) {
	m := &model{tagUI: &tagUI{phase: "menu", inventory: tagInventory{tags: map[string]tag{}}}}
	if m.tagUI.selected != 0 {
		t.Fatal("Auto should be selected")
	}
	m.tagKey("up")
	m.tagKey("\r")
	if m.tagUI.phase != "manual-kind" {
		t.Fatal(m.tagUI.phase)
	}
	m.tagKey("\r")
	if m.tagUI.input != "v0.0.1" {
		t.Fatal(m.tagUI.input)
	}
	m.tagKey("\x15")
	for _, k := range "v2.0.0" {
		m.tagKey(string(k))
	}
	if m.tagUI.input != "v2.0.0" {
		t.Fatal(m.tagUI.input)
	}
	m.tagKey("\x1b")
	if m.tagUI != nil {
		t.Fatal("Escape should leave")
	}
}

func TestPromotionStopsOnFailedUAT(t *testing.T) {
	posted := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			posted = true
			w.WriteHeader(http.StatusCreated)
		case strings.HasSuffix(r.URL.Path, "/repository/tags"):
			fmt.Fprint(w, `[{"name":"uat-v2.0.0","commit":{"id":"abc"}}]`)
		case strings.HasSuffix(r.URL.Path, "/pipelines"):
			fmt.Fprint(w, `[{"id":1,"ref":"uat-v2.0.0","sha":"abc"}]`)
		case strings.HasSuffix(r.URL.Path, "/pipelines/1"):
			fmt.Fprint(w, `{"id":1,"ref":"uat-v2.0.0","sha":"abc","tag":true,"status":"failed"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	m := &model{c: newClient(&config{host: s.URL})}
	err := m.runTag(context.Background(), 1, tagPlan{mode: "auto", name: "v2.0.0", sha: "abc"}, func(string) {})
	if err == nil || posted {
		t.Fatalf("err=%v posted=%v", err, posted)
	}
}

func TestPromotionOfExistingUATUsesItsCommit(t *testing.T) {
	posted := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			r.ParseForm()
			if r.Form.Get("tag_name") != "v1.2.3" || r.Form.Get("ref") != "abc" {
				t.Errorf("wrong tag request: %v", r.Form)
			}
			posted = true
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"name":"v1.2.3","commit":{"id":"abc"}}`)
		case strings.HasSuffix(r.URL.Path, "/repository/tags"):
			fmt.Fprint(w, `[{"name":"uat-v1.2.3","commit":{"id":"abc"}}]`)
		case strings.HasSuffix(r.URL.Path, "/pipelines"):
			fmt.Fprint(w, `[{"id":1,"ref":"uat-v1.2.3","sha":"abc"}]`)
		case strings.HasSuffix(r.URL.Path, "/pipelines/1"):
			fmt.Fprint(w, `{"id":1,"ref":"uat-v1.2.3","sha":"abc","tag":true,"status":"success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer s.Close()
	m := &model{c: newClient(&config{host: s.URL})}
	err := m.runTag(context.Background(), 1, tagPlan{mode: "auto", name: "v1.2.3", sha: "abc"}, func(string) {})
	if err != nil || !posted {
		t.Fatalf("err=%v posted=%v", err, posted)
	}
}
