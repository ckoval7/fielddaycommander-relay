---
name: branch-workflow
description: Use this skill for any of these tasks in Field Day Commander Relay: choosing a branch; starting, committing or merging work; merging feature branches into dev; or running Go, tests, gofmt, go vet or reuse lint.
---

# Branch Workflow

Work flows **feature branch → `dev` → `main`**.

| Branch | Holds | Only merges in from |
|--------|-------|---------------------|
| `main` | The latest release; tags live here | `dev`, fast-forward only, at release time |
| `dev` | Finished work for the next release | Feature branches |

## Rules

1. Work on a new feature branch off an up-to-date `dev`. Never commit directly to `dev` or `main`.
2. Never merge anything but `dev` into `main`, and only as part of a release (see the `release` skill). Never merge `main` into `dev`.
3. **Never push and never offer to push.** Commits, merges and tags stay local, and the user pushes when they're ready. Never rewrite the history of `dev` or `main`.

## Merging a feature branch into dev

Run the full checks on the branch first and fix what fails, including flaky tests they turn up. Then count the branch's commits. More than one: squash. Exactly one: a regular merge.

```bash
git switch dev
git merge --squash fix/thing && git commit      # multi-commit branch; write one summary commit
git merge --no-ff fix/thing                     # single-commit branch
git branch -D fix/thing                         # -D is needed after a squash
```

## Running Go tooling

The host doesn't have Go, so run it in a one-off container from the repo root. Once `go.mod` exists, use the image tag matching its `go` line instead of `golang:1`.

```bash
gorun() { mkdir -p "$HOME/.cache/fdc-relay-go"; docker run --rm -v "$PWD":/src -w /src -u "$(id -u):$(id -g)" \
  -v "$HOME/.cache/fdc-relay-go":/cache -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
  golang:1 "$@"; }

gorun go test ./path/to/pkg        # while working: the packages you touched
gorun go test -race ./...          # full suite, before merging into dev
gorun go vet ./...
gorun gofmt -l .                   # must print nothing
```

Run the Windows build before merging too, since Windows is the primary target:

```bash
docker run --rm -v "$PWD":/src -w /src -u "$(id -u):$(id -g)" -v "$HOME/.cache/fdc-relay-go":/cache \
  -e HOME=/tmp -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod \
  -e CGO_ENABLED=0 -e GOOS=windows -e GOARCH=amd64 golang:1 go build ./...
```

## Checking licensing

Every new file needs an SPDX header or a `REUSE.toml` entry. This must pass:

```bash
docker run --rm -v "$PWD":/data fsfe/reuse lint
```
