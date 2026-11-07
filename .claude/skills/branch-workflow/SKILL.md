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

## GitHub protection

`.github/rulesets/main.json` and `dev.json` define the rulesets: no deletion, no force-push and changes by PR. Repo admins bypass them, which is how the user's pushes to `dev` and `main` still work. `main` also requires `SonarCloud Code Analysis`, pinned to the app's integration id 12526 so nothing else can post a passing check under that name. `dev` doesn't, because scans only run on PRs into `main`.

To change a ruleset, edit its file, find its id with `gh api repos/ckoval7/fielddaycommander-relay/rulesets`, and `PUT` the file to `.../rulesets/<id>`.

## Merging a feature branch into dev

Run the full checks on the branch first and fix what fails, including flaky tests they turn up. Then count the branch's commits. More than one: squash. Exactly one: a regular merge.

```bash
git switch dev
git merge --squash fix/thing && git commit      # multi-commit branch; write one summary commit
git merge --no-ff fix/thing                     # single-commit branch
git branch -D fix/thing                         # -D is needed after a squash
```

## Running Go tooling

The host doesn't have Go, so `scripts/go.sh` runs it in a container that sees only the repo and Go's cache folder. It reads the image tag from `go.mod`. Run it as its own command, not chained or inside a shell function, so the allow rule in `.claude/settings.local.json` matches it and it runs without a prompt.

```bash
scripts/go.sh test ./path/to/pkg        # while working: the packages you touched
scripts/go.sh test -race ./...          # full suite, before merging into dev
scripts/go.sh vet ./...
scripts/go.sh gofmt -l .                # must print nothing
```

Run the Windows build before merging too, since Windows is the primary target:

```bash
scripts/go.sh windows build ./...
```

## Checking licensing

Every new file needs an SPDX header or a `REUSE.toml` entry. This must pass:

```bash
docker run --rm -v "$PWD":/data fsfe/reuse lint
```
