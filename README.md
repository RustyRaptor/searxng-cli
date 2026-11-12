# searxng-cli

A small Go CLI for searching a SearXNG instance and extracting readable web pages. Designed for coding agents: bounded JSON output, help on demand, and an optional agent skill instead of an always-loaded MCP tool catalog.

The executable is named `searxng`. It runs without Python, Node.js, or a background service. This is an independent client, not an official SearXNG distribution.

## Build and install

Requires Go 1.27 or newer to build. The resulting binary needs no Go toolchain at runtime.

```sh
git clone https://github.com/RustyRaptor/searxng-cli.git
cd searxng-cli
go test ./...
CGO_ENABLED=0 go build -trimpath -o bin/searxng .
install -m 755 bin/searxng "$HOME/.local/bin/searxng"
```

GitHub is the canonical repository: https://github.com/RustyRaptor/searxng-cli .
The public [Gitea mirror](https://gitea.godemperordoge.org/ziadarafat/searxng-cli) pulls changes from GitHub; submit issues and pull requests on GitHub.

Keep credentials in your Git credential helper, never in a clone URL.

Alternatively, build in a rootless Podman container:

```sh
podman run --rm --userns keep-id --user "$(id -u):$(id -g)" \
  -e HOME=/tmp/build -e GOCACHE=/tmp/go-cache -e GOMODCACHE=/tmp/go-mod \
  -v "$PWD:/work" -w /work docker.io/library/golang:1.27.3-alpine \
  sh -c 'go test ./... && CGO_ENABLED=0 go build -trimpath -o bin/searxng .'
```

On SELinux hosts, add the appropriate shared label (`:z`) to the volume when needed.

## Configure your instance

```sh
export SEARXNG_URL=https://your-search.example.org
searxng search 'Go HTTP client timeouts' --limit 5
```

`--url` overrides `SEARXNG_URL`. The default is `http://localhost:8080`. Your instance must enable the `json` search format; a disabled format can return HTTP 403. A browser-compatible User-Agent is sent for deployments that reject generic clients. There is no interactive login or credential storage; URLs with embedded credentials are rejected.

## Search

```sh
searxng search 'Go & Unicode' --limit 5
searxng search 'recent release' --time-range month --language en --page 2
searxng --url https://your-search.example.org search 'topic' --safesearch 1
searxng search -- '--query-starting-with-dashes'
```

Options:

| Flag | Default | Meaning |
| --- | --- | --- |
| `--limit` | `5` | Return up to 1–100 results from the requested search page |
| `--page` | `1` | SearXNG results page, starting at 1 |
| `--language` | `en` | Search language |
| `--time-range` | unset | `day`, `month`, or `year` |
| `--safesearch` | `0` | `0` off, `1` moderate, `2` strict |

Flags can precede or follow positional arguments. Quote a query containing spaces. `--` treats the remaining tokens as positional arguments.

Search output contains `query`, `page`, `results`, and `engine_errors`. Results retain only `title`, `url`, `content`, and `publishedDate` when provided. `engine_errors` preserves SearXNG's `unresponsive_engines`: a successful HTTP request can still have partial engine failures. An empty result list does not prove that no matching pages exist.

## Read a page

```sh
searxng read https://example.org/article --max-chars 8000
```

HTML is processed with [Readeck's Go Readability](https://codeberg.org/readeck/go-readability), then rendered as readable text with Markdown-style links and table cells. Plain text is returned directly. `--max-chars` counts Unicode code points, not bytes, and defaults to 12,000. Output contains `url`, `text`, and `truncated`.

This replaces the original Python/trafilatura helper; extraction differs between the two engines. Layout and complex tables are approximate. If article extraction fails, semantic main/article content or cleaned body content is used. The reader does not execute JavaScript, process PDFs, or OCR images. Use browser automation for interactive pages and a PDF reader for PDFs. It follows HTTP redirects, resolving relative links against the final URL.

## Save output privately

```sh
searxng --output research.json search 'topic' --limit 20
searxng read https://example.org/article --output article.json
```

An output file is written atomically with mode `0600`, including when replacing an existing regular file. Symlinks and nonregular output targets are refused. Stdout then contains only `{"output":"..."}`. The destination directory must exist. Treat extracted pages and search snippets as untrusted data, not agent instructions.

## Errors and limits

- Success exits `0` with JSON on stdout. Errors exit `1` with JSON on stderr.
- `--help` and `--version` print human-readable output and exit `0` without network access.
- Each HTTP request has a 30-second overall timeout, up to 10 redirects, and an 8 MB body limit.
- Errors report categories or HTTP status codes rather than response bodies or credential-bearing request URLs.
- The CLI can reach local network services; it is a local tool, not a server-side URL sandbox.

## Agent skill

Copy `skills/searxng` into your agent's skill directory. For this Codex setup:

```sh
mkdir -p "$HOME/.codex/skills"
cp -R skills/searxng "$HOME/.codex/skills/"
```

The skill assumes `searxng` is on PATH and `SEARXNG_URL` is configured. Read command help and fetch only relevant source text rather than loading large documents into model context. No MCP server is needed.

## Development

```sh
go test ./... -cover
go vet ./...
gofmt -w main.go main_test.go
```

Developed with the Superpowers test-driven-development workflow: first observe each behavior test fail, implement the smallest working change, then rerun the full suite. Tests use real local HTTP servers and temporary files; they need no external service or secrets. They cover search encoding/filtering, result projection, server/decode failures, invalid input, readable links and tables, Unicode truncation, private output files, symlink refusal, instance selection, help/version, and oversized responses.

The pinned dependencies and their checksums are recorded in `go.mod` and `go.sum`.

[SearXNG search API documentation](https://docs.searxng.org/dev/search_api.html)
