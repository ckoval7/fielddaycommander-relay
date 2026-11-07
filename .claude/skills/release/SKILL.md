---
name: release
description: Use this skill to release Field Day Commander Relay. Triggers include a version bump, a release, tagging (vYY.MM.N) and "ship it". It runs vulnerability scans, updates dependencies, bumps the version, fast-forwards main and tags. Never bump or tag without completing the earlier steps.
---

# Release

Run the steps in order, and don't bump the version early. Relay versions are independent of the app's.

The build workflow makes both Windows versions with each build: the installer (`scripts/build-installer.sh`, `installer/relay.nsi`) and `relay-portable.exe`. Code signing isn't set up yet, so Windows warns about the installer. When signing is set up, add it here. Don't improvise it during a release.

The `branch-workflow` skill has the `gorun` helper and the merge rules.

## 0. Prepare

```bash
git status --short                                   # must be clean
git switch dev
git merge-base --is-ancestor main dev && echo "main can fast-forward"
```

If the tree has uncommitted changes or `main` can't fast-forward, stop and ask.

Work out the version. The scheme is `vYY.MM.N`, where `N` counts releases within the month. If `VERSION` has the current year and month, increment `N`. Otherwise use `YY.MM.1` for the current month. Check `git tag` so you don't reuse a tagged number.

Fixes in steps 1 and 2 go on short branches off `dev` (`fix/deps-<pkg>`, `chore/deps-update`), merged as `branch-workflow` describes.

## 1. Vulnerability scans (iterate until clean)

```bash
gorun go run golang.org/x/vuln/cmd/govulncheck@latest ./...
docker run --rm -v "$PWD":/project:ro aquasec/trivy:latest fs --scanners vuln --quiet /project
```

Upgrade the affected module and re-scan. Commit as `fix(deps): upgrade <module> to <ver> (<CVE or GHSA id>)`.

Then run the Sonar scan in `branch-workflow` and fix what it finds in the same way, until it shows no issues or hotspots.

## 2. Update dependencies

```bash
gorun go list -m -u all     # list available updates first
gorun go get -u ./... && gorun go mod tidy
```

Major version bumps change the import path and possibly behavior, so the user must approve them. List them with what would change, ask, and only bump the ones they approve. Run the full checks from `branch-workflow`, re-run step 1, and commit as `chore(deps): update go modules`.

## 3. Version bump

1. `git switch -c release/vYY.MM.N` off `dev`.
2. Check **Unreleased** in `CHANGELOG.md` against `git log --no-merges <last tag>..dev`. Every change a user would notice needs an entry, written through the `prose-writer` agent. A fix for a bug that never reached a release gets no entry. If the section is empty, ask whether there's anything to release.
3. Change exactly two files:
   - `VERSION`: the new version, without the `v`
   - `CHANGELOG.md`: rename `## Unreleased` to `## YY.MM.N - YYYY-MM-DD` and add a fresh, empty `## Unreleased` above it.
4. Commit as `chore(release): vYY.MM.N`, merge into `dev` with `--no-ff`, and delete the branch.

## 4. Ship

1. Fast-forward `main`: `git switch main && git merge --ff-only dev`, then `git switch dev`.
2. Create the annotated tag on `main`'s HEAD: `git tag -a vYY.MM.N -m vYY.MM.N`.
3. Stop and tell the user that `dev`, `main` and the tag are ready to push. Never push them yourself unless the user asks in this session, and never force-push.
