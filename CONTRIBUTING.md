# Contributing

Glad you're here. groundcontrol is deliberately small — one flat Go package, two dependencies, three static frontend files — and contributions that keep it that way are the ones that land.

## What you need

- [Go 1.24+](https://go.dev)
- git
- the [Claude Code CLI](https://claude.com/claude-code), logged in — only needed to actually *run* the tower and launch sessions; building and testing work without it

Linux and macOS only. The PTY layer uses unix syscalls, so Windows won't build — that's known, not a bug to file.

## Build and run

```bash
git clone https://github.com/connorbell133/groundcontrol.git
cd groundcontrol
go build -o groundcontrol ./cmd/groundcontrol
```

To run it, copy [`config.example.json`](config.example.json) to `config.json` in the directory you'll run from, point `roots` at some folders, set an `authToken` (`openssl rand -hex 16`), then:

```bash
go run ./cmd/groundcontrol
```

`public/` is embedded with `go:embed`, so frontend edits need a rebuild — stop and re-run `go run ./cmd/groundcontrol` to see them.

## Before you push

```bash
gofmt -l .           # should print nothing
go vet ./...
go test -race ./...
```

CI runs exactly these (plus `staticcheck` and a `go mod tidy -diff` check) on every push and PR, so running them locally saves you a round-trip. Wiring up the pre-commit hook catches most of it automatically, along with secrets and broken JSON/YAML:

```bash
git config core.hooksPath .githooks
```

## Pull requests

- **Small and focused.** One fix or one feature per PR. If your diff needs three paragraphs of framing, it probably wants to be three PRs.
- **Comment the *why*, not the *what*.** This codebase explains its reasons — why there's no `WriteTimeout`, why directory listings 404. `// increment i` comments get asked to leave; `// bounded so a wedged PTY can't hold shutdown hostage` comments get to stay. Match that.
- **API changes update the docs.** The HTTP API is documented twice, on purpose: [docs/api.md](docs/api.md) is the guide for humans, [docs/openapi.yaml](docs/openapi.yaml) is the machine contract. If you touch the API, both change in the same PR — a PR that changes one without the other isn't done.
- Open an issue first for anything bigger than a bug fix. See [what it deliberately isn't](README.md#what-it-deliberately-isnt) before proposing a chat UI.
