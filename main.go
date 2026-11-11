// Command searxng searches SearXNG and extracts readable web pages as JSON.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html"
)

var version = "dev"

type options struct {
	base, language, timeRange, safe string
	limit, page, maxChars           int
}

// Run executes a single command without changing process-global state.
func Run(args []string, out, errOut io.Writer) int {
	fail := func(message string) int {
		_ = json.NewEncoder(errOut).Encode(map[string]string{"error": message})
		return 1
	}
	if len(args) == 1 && args[0] == "--version" {
		fmt.Fprintln(out, "searxng "+version)
		return 0
	}
	help := len(args) == 0
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			help = true
		}
	}
	if help {
		fmt.Fprintln(out, `Usage: searxng [--url URL] [--output FILE] search QUERY [options]
       searxng [--output FILE] read URL [--max-chars N]
Search options: --limit 5 (1..100), --page 1, --language en,
                --time-range day|month|year, --safesearch 0|1|2
Read options: --max-chars 12000
Global: --url (or SEARXNG_URL; default http://localhost:8080), --output, --version
Flags may appear before or after the query. Use -- before dash-prefixed queries.
Success JSON goes to stdout; failures return JSON on stderr and exit 1.`)
		return 0
	}
	fs := flag.NewFlagSet("searxng", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := options{}
	defaultURL := os.Getenv("SEARXNG_URL")
	if defaultURL == "" {
		defaultURL = "http://localhost:8080"
	}
	fs.StringVar(&o.base, "url", defaultURL, "SearXNG instance URL")
	output := fs.String("output", "", "Save JSON privately to a file")
	fs.IntVar(&o.limit, "limit", 5, "Maximum search results (1..100)")
	fs.IntVar(&o.page, "page", 1, "Search page")
	fs.StringVar(&o.language, "language", "en", "Search language")
	fs.StringVar(&o.timeRange, "time-range", "", "day, month, or year")
	fs.StringVar(&o.safe, "safesearch", "0", "0, 1, or 2")
	fs.IntVar(&o.maxChars, "max-chars", 12000, "Maximum extracted Unicode characters")
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && a != "--help" && a != "-h" && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			positionals = append(positionals, a)
		}
	}
	if err := fs.Parse(append(flags, positionals...)); err != nil {
		return fail("invalid arguments")
	}
	pos := fs.Args()
	if len(pos) != 2 || (pos[0] != "search" && pos[0] != "read") {
		return fail("expected search QUERY or read URL")
	}
	if o.limit < 1 || o.limit > 100 || o.page < 1 || o.maxChars < 1 {
		return fail("limit must be 1..100; page and max-chars must be positive")
	}
	if o.timeRange != "" && o.timeRange != "day" && o.timeRange != "month" && o.timeRange != "year" {
		return fail("invalid time-range")
	}
	if o.safe != "0" && o.safe != "1" && o.safe != "2" {
		return fail("invalid safesearch")
	}
	target := pos[1]
	if pos[0] == "search" {
		u, err := checkedURL(o.base)
		if err != nil {
			return fail(err.Error())
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/search"
		q := u.Query()
		q.Set("q", pos[1])
		q.Set("format", "json")
		q.Set("pageno", strconv.Itoa(o.page))
		q.Set("language", o.language)
		q.Set("safesearch", o.safe)
		if o.timeRange != "" {
			q.Set("time_range", o.timeRange)
		}
		u.RawQuery = q.Encode()
		target = u.String()
	}
	body, contentType, finalURL, err := fetch(target)
	if err != nil {
		return fail(err.Error())
	}
	var result any
	if pos[0] == "search" {
		var data struct {
			Results []map[string]any `json:"results"`
			Errors  []any            `json:"unresponsive_engines"`
		}
		if err = json.Unmarshal(body, &data); err != nil {
			return fail("invalid JSON from search server")
		}
		results := make([]map[string]any, 0)
		for i, item := range data.Results {
			if i >= o.limit {
				break
			}
			r := map[string]any{}
			for _, k := range []string{"title", "url", "content", "publishedDate"} {
				if v, ok := item[k]; ok && v != nil {
					r[k] = v
				}
			}
			results = append(results, r)
		}
		if data.Errors == nil {
			data.Errors = []any{}
		}
		result = map[string]any{"query": pos[1], "page": o.page, "results": results, "engine_errors": data.Errors}
	} else {
		text := string(body)
		if strings.Contains(strings.ToLower(contentType), "html") {
			pageURL, _ := url.Parse(finalURL)
			article, extractErr := readability.FromReader(bytes.NewReader(body), pageURL)
			node, parseErr := html.Parse(bytes.NewReader(body))
			if parseErr != nil {
				return fail("cannot parse page")
			}
			if extractErr == nil && article.Node != nil {
				node = article.Node
			}
			// Prefer semantic content on small pages where article extraction is inconclusive.
			if main := findContent(node); main != nil {
				node = main
			}
			text = renderText(node, pageURL)
		}
		runes := []rune(text)
		truncated := len(runes) > o.maxChars
		if truncated {
			text = string(runes[:o.maxChars])
		}
		result = map[string]any{"url": target, "text": text, "truncated": truncated}
	}
	var encoded bytes.Buffer
	enc := json.NewEncoder(&encoded)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err = enc.Encode(result); err != nil {
		return fail("cannot encode output")
	}
	if *output != "" {
		if err = writePrivate(*output, encoded.Bytes()); err != nil {
			return fail("cannot safely write output file")
		}
		if err = json.NewEncoder(out).Encode(map[string]string{"output": *output}); err != nil {
			return fail("output failed")
		}
	} else if _, err = out.Write(encoded.Bytes()); err != nil {
		return fail("output failed")
	}
	return 0
}

func checkedURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, fmt.Errorf("expected HTTP(S) URL without embedded credentials")
	}
	return u, nil
}

func fetch(target string) ([]byte, string, string, error) {
	if _, err := checkedURL(target); err != nil {
		return nil, "", "", err
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return nil, "", "", fmt.Errorf("invalid request")
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; SearXNGCLI/1.0)")
	req.Header.Set("Accept", "application/json,text/html,text/plain")
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		_, err := checkedURL(req.URL.String())
		return err
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", "", fmt.Errorf("request failed (check connectivity and timeout)")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8_000_001))
	if err != nil {
		return nil, "", "", fmt.Errorf("cannot read response")
	}

	if len(body) > 8_000_000 {
		return nil, "", "", fmt.Errorf("response exceeds 8 MB")
	}
	return body, resp.Header.Get("Content-Type"), resp.Request.URL.String(), nil
}

func findContent(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && (n.Data == "main" || n.Data == "article") {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findContent(c); found != nil {
			return found
		}
	}
	return nil
}

func renderText(root *html.Node, base *url.URL) string {
	var walk func(*html.Node) string
	walk = func(n *html.Node) string {
		if n.Type == html.TextNode {
			return strings.Join(strings.Fields(n.Data), " ")
		}
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "nav", "footer", "header", "aside", "noscript", "head":
				return ""
			}
		}
		var b strings.Builder
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			s := walk(c)
			if s != "" {
				b.WriteString(s)
				b.WriteString(" ")
			}
		}
		text := strings.TrimSpace(b.String())
		if n.Type == html.ElementNode {
			if n.Data == "a" {
				for _, a := range n.Attr {
					if a.Key == "href" {
						u, err := url.Parse(a.Val)
						if err == nil {
							u = base.ResolveReference(u)
							if u.Scheme == "http" || u.Scheme == "https" {
								return "[" + text + "](" + u.String() + ")"
							}
						}
					}
				}
			}
			switch n.Data {
			case "p", "div", "h1", "h2", "h3", "h4", "li", "tr", "section", "article", "main":
				return text + "\n"
			case "th", "td":
				return text + " |"
			case "br":
				return "\n"
			}
		}
		return text
	}
	return strings.TrimSpace(walk(root))
}

// writePrivate atomically replaces regular output files without following symlinks.
func writePrivate(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("output must be regular file")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".searxng-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func main() { os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr)) }
