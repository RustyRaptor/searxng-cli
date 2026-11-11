package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Catches missing query encoding, dropped filters, excessive output, or missing engine errors.
func TestSearchEncodesFiltersAndLimitsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Errorf("path %s", r.URL.Path)
		}
		for k, want := range map[string]string{"q": "Go & café", "format": "json", "pageno": "2", "language": "fr", "time_range": "month", "safesearch": "1"} {
			if got := r.URL.Query().Get(k); got != want {
				t.Errorf("%s=%q, want %q", k, got, want)
			}
		}
		if r.Header.Get("User-Agent") == "" {
			t.Error("missing user agent")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"First","url":"https://example.org/1","content":"A","irrelevant":"omit"},{"title":"Second"}],"unresponsive_engines":[["bing","timeout"]]}`))
	}))
	defer server.Close()
	var out, errs bytes.Buffer
	code := Run([]string{"--url", server.URL, "search", "Go & café", "--limit", "1", "--page", "2", "--language", "fr", "--time-range", "month", "--safesearch", "1"}, &out, &errs)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs.String())
	}
	var got struct {
		Query        string
		Page         int
		Results      []map[string]any
		EngineErrors []any `json:"engine_errors"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Query != "Go & café" || got.Page != 2 || len(got.Results) != 1 || got.Results[0]["title"] != "First" || len(got.EngineErrors) != 1 {
		t.Fatalf("unexpected response %s", out.String())
	}
	if _, ok := got.Results[0]["irrelevant"]; ok {
		t.Fatal("leaked unrequested fields")
	}
}

// Catches ignored HTTP status, invalid payload success, and oversized response acceptance.
func TestSearchReportsHTTPAndDecodeFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"http", 403, `secret response`, `403`},
		{"json", 200, `not-json`, `invalid JSON`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); w.Write([]byte(tc.body)) }))
			defer s.Close()
			var out, errs bytes.Buffer
			if code := Run([]string{"--url", s.URL, "search", "test"}, &out, &errs); code == 0 {
				t.Fatalf("accepted failure: %s", out.String())
			}
			if !bytes.Contains(errs.Bytes(), []byte(tc.want)) {
				t.Fatalf("missing error category %q: %s", tc.want, errs.String())
			}
			if bytes.Contains(errs.Bytes(), []byte("secret response")) {
				t.Fatal("response body leaked")
			}
		})
	}
}

// Catches invalid flags reaching the network, including negative slice bounds and URL credential leaks.
func TestInvalidInputRejectedBeforeRequest(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("invalid input reached HTTP server")
		w.Write([]byte(`{"results":[]}`))
	}))
	defer s.Close()
	t.Setenv("SEARXNG_URL", s.URL)
	for _, args := range [][]string{
		{"search", "test", "--limit", "0"}, {"search", "test", "--limit", "101"},
		{"search", "test", "--page", "0"}, {"search", "test", "--time-range", "century"},
		{"search", "test", "--safesearch", "3"}, {"--url", "ftp://example.com", "search", "test"},
		{"--url", "https://user:private@example.com", "search", "test"},
	} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			var out, errs bytes.Buffer
			if code := Run(args, &out, &errs); code == 0 {
				t.Fatal("accepted invalid input")
			}
			if errs.Len() == 0 {
				t.Fatal("no diagnostic")
			}
			if bytes.Contains(errs.Bytes(), []byte("private")) {
				t.Fatal("credential leaked")
			}
		})
	}
}

// Catches lost Unicode, lost links/tables, script leakage, and byte-based truncation.
func TestReadExtractsContentAndTruncatesRunes(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plain" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write([]byte("é界🙂abc"))
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<html><body><nav>Navigation junk</nav><main><h1>Useful article</h1><p>A helpful <a href="/guide">guide</a> with enough useful words to explain how a search command works.</p><table><tr><th>Tool</th><th>Role</th></tr><tr><td>Go</td><td>CLI</td></tr></table></main><script>private-script</script></body></html>`))
	}))
	defer s.Close()
	var out, errs bytes.Buffer
	if code := Run([]string{"read", s.URL + "/article"}, &out, &errs); code != 0 {
		t.Fatalf("read failed: %s", errs.String())
	}
	var got struct {
		Text      string
		Truncated bool
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Useful article", "[guide](" + s.URL + "/guide)", "Tool", "CLI"} {
		if !strings.Contains(got.Text, want) {
			t.Errorf("missing %q: %s", want, got.Text)
		}
	}
	if strings.Contains(got.Text, "private-script") || strings.Contains(got.Text, "Navigation junk") {
		t.Fatalf("included clutter: %s", got.Text)
	}
	out.Reset()
	errs.Reset()
	if code := Run([]string{"read", s.URL + "/plain", "--max-chars", "3"}, &out, &errs); code != 0 {
		t.Fatal(errs.String())
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "é界🙂" || !got.Truncated {
		t.Fatalf("Unicode truncation: %+v", got)
	}
}

// Catches accidental stdout payloads or permissive files when saving private research.
func TestOutputFileIsPrivateAndContainsJSON(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"results":[],"unresponsive_engines":[]}`))
	}))
	defer s.Close()
	path := filepath.Join(t.TempDir(), "results.json")
	var out, errs bytes.Buffer
	if code := Run([]string{"--output", path, "--url", s.URL, "search", "test"}, &out, &errs); code != 0 {
		t.Fatalf("output failed: %s", errs.String())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err = json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result["query"] != "test" {
		t.Fatalf("file %s", data)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions %o", info.Mode().Perm())
	}
	var summary map[string]any
	if err = json.Unmarshal(out.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["output"] != path || summary["results"] != nil {
		t.Fatalf("stdout should contain only output path: %s", out.String())
	}
	// Existing permissive files must become private, too.
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{"search", "test", "--url", s.URL, "--output", path}, io.Discard, &errs); code != 0 {
		t.Fatal(errs.String())
	}
	info, _ = os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("existing permissions %o", info.Mode().Perm())
	}
}

// Catches following an output symlink and overwriting an unrelated file.
func TestOutputRefusesSymlinks(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"results":[]}`)) }))
	defer s.Close()
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	link := filepath.Join(dir, "link")
	if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	var errs bytes.Buffer
	if code := Run([]string{"search", "test", "--url", s.URL, "--output", link}, io.Discard, &errs); code == 0 {
		t.Fatal("followed symlink")
	}
	if !strings.Contains(errs.String(), "output") {
		t.Fatalf("expected output safety failure: %s", errs.String())
	}
	content, _ := os.ReadFile(victim)
	if string(content) != "keep" {
		t.Fatal("overwrote symlink target")
	}
}

// Catches silently ignoring the deployment URL from the environment.
func TestInstanceURLFromEnvironmentAndFlagOverride(t *testing.T) {
	hits := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.Write([]byte(`{"results":[]}`)) }))
	defer s.Close()
	t.Setenv("SEARXNG_URL", s.URL)
	var errs bytes.Buffer
	if code := Run([]string{"search", "test"}, io.Discard, &errs); code != 0 {
		t.Fatal(errs.String())
	}
	if hits != 1 {
		t.Fatalf("environment endpoint hits=%d", hits)
	}
	t.Setenv("SEARXNG_URL", "invalid")
	if code := Run([]string{"--url", s.URL, "search", "test"}, io.Discard, &errs); code != 0 {
		t.Fatal(errs.String())
	}
	if hits != 2 {
		t.Fatal("flag did not override environment")
	}
}

// Catches help/version requiring network access or reporting failure.
func TestHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"search", "--help"}, {"read", "--help"}, {"--version"}} {
		var out, errs bytes.Buffer
		if code := Run(args, &out, &errs); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, errs.String())
		}
		if out.Len() == 0 || errs.Len() != 0 {
			t.Fatalf("%v: expected stdout help/version", args)
		}
	}
}

// Catches responses that exhaust memory or silently truncate into invalid JSON.
func TestOversizedResponsesAreRejected(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, strings.Repeat("x", 8_000_001))
	}))
	defer s.Close()
	var out, errs bytes.Buffer
	if code := Run([]string{"read", s.URL}, &out, &errs); code == 0 {
		t.Fatal("accepted oversized response")
	}
	if !strings.Contains(errs.String(), "8 MB") {
		t.Fatalf("missing size error: %s", errs.String())
	}
}
