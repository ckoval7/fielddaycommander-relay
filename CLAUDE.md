# Field Day Commander Relay

A desktop relay for ham radio clubs running Field Day Commander. It runs on a logging computer, collects contacts from local logging software, and sends them securely to a Field Day Commander server that may be on another network.

The server app is a separate repo, checked out at `/home/corey/fielddaycommander`. Its `CLAUDE.md` explains the domain.

## Status

The core exists under `internal/`: contact rules (`contact`), durable queue (`queue`), API client (`api`), send loop (`sender`) and the N3FJP source (`n3fjp`). `app`, `ui`, `secret`, `settings`, `tray` and `cmd/relay` make it a program. There's no installer or start-at-login yet, and the Windows keychain and tray have only been compiled here, because this environment can't run Windows binaries. Keychain support exists only on Windows, so other builds can't enroll. The server's API is in the app repo, and `api/relay-v1.openapi.yaml` is a pinned copy of its contract.

## Stack

Go, built as a single binary with no runtime. Keep `CGO_ENABLED=0` for release builds so cross-compiling stays simple, and prefer pure-Go dependencies (`modernc.org/sqlite`, not `mattn/go-sqlite3`). Ask before adding a dependency, since every one ships to operators' computers.

The UI is a small settings and status page served on localhost, plus a tray icon.

## Where it runs

- **Windows first.** N3FJP and N1MM Logger+ are Windows programs, so that's where the relay matters most. Linux is a cheap extra build. Mac waits until someone asks for it. Keep Windows-specific code (tray, keychain, service install) behind small interfaces, but don't build or test other platforms ahead of need.
- **On small, old hardware,** sometimes on battery. Watch memory and background work.
- **On bad networks.** Internet may drop for hours during an event. The relay has to keep collecting while offline and catch up when the connection returns.
- **Installed by non-developers,** possibly on Field Day morning. No runtime to install, no ports to open, no config files to edit by hand.

## Design rules

- **Outbound only.** The relay makes outbound HTTPS or WSS connections to the server, so it works behind NAT without port forwarding. Its local UI listens on loopback only.
- **Store before sending.** Write each contact to the local queue before any attempt to send it. Losing a contact is the worst failure this program can have.
- **Retries can't create duplicates.** Each contact gets a stable idempotency key, derived from its own fields. Never use N3FJP record IDs for this: each client assigns them by counting ADDs in order, so they aren't stable.
- **TLS always verified.** Never add an option to skip certificate checks for remote servers. Self-signed club servers get certificate pinning instead.
- **Credentials.** A relay enrolls with a one-time code and receives a revocable per-relay token, stored in the OS keychain. Never write it to a log or a plain config file.
- **Local data is untrusted.** Local logger protocols have no authentication, so anything on the LAN could be talking to the relay. Validate everything before it's queued, and default to connecting only to localhost or an address the user set.

## Log sources

- **N3FJP network protocol (TCP 1000):** UTF-16LE, frames end with `\x03\x04\x07`, messages are `<BOR>…<EOR>`. There's no auth, handshake or version check. A server in TCP mode answers `OPEN` (`CLEAR`, `GET`..., `LIST`) and `CHECK` on the asking connection alone, and relays everything else to all clients. A server in file-share mode relays `OPEN` and `CHECK` too, which makes every PC beep and warn, so the relay sends one `OPEN` per connection and no `CHECK` until the server answers. The relay joins as a listening client and sends nothing else. It needs N3FJP's TCP network mode, and hasn't been run against real N3FJP software yet.
- **Other loggers:** the app's listeners in `app/Console/Commands/*ListenCommand.php` show how it handles N1MM Logger+, WSJT-X/JTDX, fldigi and ADIF-over-UDP.
- **N1MM peer network (12070), if ever joined:** never send `FUNCTIONKEY` (keys another PC's radio), `TIME` (sets its clock) or `FILE` (overwrites its data files).

Implement protocols from their wire format. Never commit third-party or decompiled source, or anything bundled from another vendor, including logos and forms.

## Server contract

Relays and servers upgrade separately, and clubs may not update either right before an event.

- The server owns the API, under a versioned path such as `/api/relay/v1`. Keep the OpenAPI spec or JSON schema in the app repo, and test against a pinned copy here.
- The relay reports its version when it connects and the server reports the range it supports, so an outdated relay can show a clear message.
- On the server, contacts should enter through `ExternalContactHandler`, the same path the local listeners use.

## License

Code is `GPL-3.0-or-later`. Documentation and images are `CC-BY-NC-SA-4.0`. The repo follows the [REUSE](https://reuse.software) spec.

Every new source file starts with an SPDX header. In Go, follow it with a blank line so it doesn't become the package doc:

```go
// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package main
```

Files that can't hold a comment go in `REUSE.toml`. `reuse lint` must pass (the `branch-workflow` skill has the command).

## Branches and tooling

- **Flow:** feature branch → `dev` → `main`. Never commit directly to `dev` or `main`.
- **Never push or offer to.** The user pushes.
- **Go runs in a container,** because the host doesn't have it.

Activate the `branch-workflow` skill before you start, commit or merge work, or run Go tooling.

## Writing

- **Commits:** Conventional Commits, describing the change in plain words (`fix(queue): keep unsent contacts after a restart`).
- **Changelog:** anything a user could notice gets a `CHANGELOG.md` entry under **Unreleased** in the same branch. Describe fixes by what the person saw and when, not what changed in the code. Skip refactors, typos and CI. Keep the file to release sections, with no intro.
- **Product name:** Field Day Commander Relay, or "the relay" after the first mention. The server is Field Day Commander. Never "FDC".
- **User-facing prose** (UI text, error and status messages, README, changelog, guides): send it to the `prose-writer` agent, which follows the `docs-prose` skill.
- Only create documentation files when asked.

## Releases

Activate the `release` skill for any version bump, release or tag.
