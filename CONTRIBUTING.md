# Repository Guidelines

## Project structure

The Go entry points are in `cmd/`, application packages and their colocated tests
in `internal/`, and the embedded React console in `web/`. Platform installers and
helper tests live in `packaging/`. Reusable GitHub Actions adapters are in
`integrations/github-actions/`; optional AWS templates and synthetic examples
are in `infrastructure/priority-queue/`. Operational guides are in `docs/`.

## Build and development

Use Go 1.25+ and Node.js 22+ with npm. Build console assets before compiling Go:

```sh
(cd web && npm ci && npm run build)
go build -buildvcs=false -o sluicework ./cmd/sluicework
```

Run `npm run dev --prefix web` for console development. Build a specific installer
with `bash packaging/build-release.sh release linux/arm64`; supported targets
also include `linux/amd64`, `darwin/arm64` and `darwin/amd64`.

## Style and naming

Use `gofmt` for Go and follow existing TypeScript/React conventions in each
module. Keep Go tests in `*_test.go`, Node tests in `*.test.mjs`, and Python
installer tests in `test_*.py`. Use descriptive names and small modules. Keep
lockfiles current and generated assets, dependencies and local state untracked.

## Testing

Run `go test -race -buildvcs=false ./...` and `go vet -buildvcs=false ./...`.
Run the Node suites with `node --test integrations/github-actions/*.test.mjs
integrations/github-actions/priority/*.test.mjs` on one command line. Use
`python3 -B -m unittest discover -s packaging/macos/tests -p 'test_*.py'`
and the equivalent `packaging/linux/tests` command for installer helpers.
The console build includes TypeScript checks. Native Linux isolation/systemd
checks require disposable Linux hosts; Darwin execution requires a trusted Mac.

## Commits and pull requests

Use focused commits with imperative subjects, such as `Fix worker lease renewal`.
Explain the behavior change, link related issues, report validation and disclose
checks that were unavailable. Include synthetic screenshots for interface
changes. Public CI uses hosted Linux; do not add hosted macOS jobs or private
self-hosted selectors.

## Configuration and publication

Never commit credentials, private deployment files or operational screenshots.
Use synthetic IDs and `example-org` repositories in examples. Check source and
artifacts for account details before sharing; do not copy private Git history.
Run `python3 scripts/check-public-source.py` before committing. Public CI also
checks reachable history. See [configuration ownership](docs/configuration.md)
for explicit environment files and private deployment storage.
Use the repository's [public-source review skill](.agents/skills/review-public-source/SKILL.md)
when reviewing changes for publication, including organizational infrastructure
details in source, history, PR text and generated artifacts.
