# Contributing to Field Day Commander Relay

Start a branch from an up-to-date `dev` and open your pull request against `dev`. Keep it to one fix or feature. Use a name such as `fix/...`, `feat/...`, `chore/...`, or `docs/...`, and write Conventional Commits messages, such as `fix(queue): keep unsent contacts after a restart`.

## Before opening a pull request

Add or update tests for changed behavior. From the repo root, run the checks below with Docker available. `scripts/go.sh` runs Go in a container using the Go version in `go.mod`.

```sh
scripts/go.sh test -race ./...
scripts/go.sh vet ./...
scripts/go.sh gofmt -l .
scripts/go.sh windows build ./...
docker run --rm -v "$PWD":/data fsfe/reuse lint
```

The formatting check should print nothing. To try the change on Windows, run `scripts/build-windows.sh`. It creates `dist/relay-portable.exe` and `dist/relay-setup.exe`. The installer build uses NSIS's `makensis` if available, or a container otherwise.

Bring the latest `dev` into your branch before review, and update it again if `dev` moves before merging. Describe what changed and why in the PR. Include screenshots for visible changes. Ask before adding a dependency, since it will ship to operators' computers. New files need an SPDX header or coverage in `REUSE.toml`.

If someone using the relay would notice the change, add an entry under Unreleased in [CHANGELOG.md](CHANGELOG.md). Use Added, Changed, Fixed, Removed, or Security. Describe a fix by what the person saw and when it happened. Leave out refactors, wording changes, CI work, and fixes for bugs that never reached a release.

## Branches and checks

Work goes from feature branches to `dev`, then from `dev` to `main` at release time. A PR opened against `main` from a feature branch is moved to `dev` by the `source-branch` check. Only this repository's `dev` may merge into `main`, and `main` must never merge into `dev`. Both branches have rulesets requiring PRs and preventing deletion and force-pushes, with an administrator bypass.

The branch check runs on PRs into both branches. Tests with race detection and coverage run on PRs into `main`. Those PRs also run SonarCloud for branches in this repository. Fork PRs run the tests without the scan. The `main` ruleset requires `SonarCloud Code Analysis`. PRs into `dev` don't run that test-and-scan workflow, so run the local checks before review.

A PR with multiple commits is squash-merged into `dev`. A single-commit PR gets a regular merge.

## Releases

The maintainer prepares releases from `dev`, runs vulnerability and quality scans, updates dependencies, and fast-forwards `main` to `dev`. Versions use `vYY.MM.N`, independently of Field Day Commander.

The Build workflow runs on pushes to `main` and `v*` tags. A tag creates a draft GitHub release with the Windows installer, Windows portable version, Linux build, and checksums. Its notes come from the matching changelog section. The maintainer checks and publishes the draft.

For vulnerabilities, follow [SECURITY.md](SECURITY.md) instead of opening a public issue or PR.
