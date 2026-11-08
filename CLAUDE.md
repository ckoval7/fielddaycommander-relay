# Field Day Commander Relay

A desktop relay for ham radio clubs running Field Day Commander. It runs on a logging computer, collects contacts from local logging software, and sends them securely to a Field Day Commander server that may be on another network.

The server app is a separate repo, checked out at `/home/corey/fielddaycommander`. Its `CLAUDE.md` explains the domain.

## Status

The core exists under `internal/`: contact rules (`contact`), durable queue (`queue`), API client (`api`), send loop (`sender`) and the N3FJP source (`n3fjp`). `app`, `ui`, `secret`, `settings`, `tray` and `cmd/relay` make it a program, `feed` brings server changes back for two-way sync, `logfile` (with `adif`) picks contacts up from log files on disk, and `udplog` receives WSJT-X and plain ADIF over UDP. `autostart` starts the relay at sign-in (not boot, since the token, tray and file window all belong to the signed-in person). `instance` keeps a second copy from running, and Windows has two versions: `installer/` (NSIS, per person, unsigned) and a portable build (`-X main.portable=true`) that keeps its data in `relay-data` beside the program, has its own keychain entry per folder, and never adds a startup entry. The Windows keychain, startup entry, tray and installer have only been compiled here, because this environment can't run Windows binaries. Keychain and start-at-sign-in support exist only on Windows, so other builds can't enroll. The server's API is in the app repo, and `api/relay-v1.openapi.yaml` is a pinned copy of its contract.

## Stack

Go, built as a single binary with no runtime. Keep `CGO_ENABLED=0` for release builds so cross-compiling stays simple, and prefer pure-Go dependencies (`modernc.org/sqlite`, not `mattn/go-sqlite3`). Ask before adding a dependency, since every one ships to operators' computers.

The UI is a small settings and status page served on localhost, plus a tray icon. On Windows it opens in its own Edge app window (`appwindow`), found again by its exact title so it never piles up tabs, with the default browser as the fallback. It uses Go templates with Tailwind and daisyUI, with the same themes as Field Day Commander. The stylesheet is compiled and committed, so a plain Go build needs no Node.js. After changing `internal/ui/styles/app.css` or the page's classes, run `scripts/build-css.cjs` (versions are in `package.json`). `internal/ui/styles/theme.css` is generated from the app's stylesheet by `scripts/sync-theme.cjs`, so change colors in the app, and `--check` reports any drift.

## Where it runs

- **Windows first.** N3FJP and N1MM Logger+ are Windows programs, so that's where the relay matters most. Linux is a cheap extra build. Mac waits until someone asks for it. Keep Windows-specific code (tray, keychain, service install) behind small interfaces, but don't build or test other platforms ahead of need.
- **On small, old hardware,** sometimes on battery. Watch memory and background work.
- **On bad networks.** Internet may drop for hours during an event. The relay has to keep collecting while offline and catch up when the connection returns.
- **Installed by non-developers,** possibly on Field Day morning. No runtime to install, no ports to open, no config files to edit by hand.

## Design rules

- **Outbound only to the server.** The relay makes outbound HTTPS or WSS connections to the server, so it works behind NAT without port forwarding. Its local UI listens on loopback only. As an N3FJP network server it listens on loopback too, and on the LAN only after someone turns that on, because N3FJP has no password. Never suggest forwarding that port.
- **Store before sending.** Write each contact to the local queue before any attempt to send it. Losing a contact is the worst failure this program can have.
- **Retries can't create duplicates.** Each contact gets a stable idempotency key, derived from its own fields. Never use N3FJP record IDs for this: each client assigns them by counting ADDs in order, so they aren't stable.
- **TLS always verified.** Never add an option to skip certificate checks for remote servers. Self-signed club servers get certificate pinning instead.
- **Credentials.** A relay enrolls with a one-time code and receives a revocable per-relay token, stored in the OS keychain. Never write it to a log or a plain config file.
- **Two-way sync compares with the server, not the logger.** A contact counts as changed only when it differs from what the server last said, and only the fields that changed reach the logger, so values the server doesn't store (a `5NN` report) are never overwritten. An unanswered upload is newer than anything the feed shows, so feed changes for that contact wait. Identity is the contact's `uuid`, which also reaches contacts logged elsewhere with no idempotency key.
- **The picked station is applied when sending.** The queue keeps what the logger said, so changing the pick re-sends nothing and reaches contacts still waiting. It fills a blank station, and replaces a named one only when the override is on. The server only says what its stations are, and creates one when a contact first names it, so a new name stays local until then. The feed never writes the station back to a logger, because a relay's own upload only sets the baseline.
- **One relay per computer.** A running relay holds a machine-wide named mutex, so a second copy opens the first one's page and stops. The installer closes a running relay through its tray window before replacing it, and never removes the data folder or token.
- **Local data is untrusted.** Local logger protocols have no authentication, so anything on the LAN could be talking to the relay. Validate everything before it's queued, and default to connecting only to localhost or an address the user set.

## Log sources

- **N3FJP network protocol (TCP 1000):** UTF-16LE, frames end with `\x03\x04\x07`, messages are `<BOR>…<EOR>`. There's no auth, handshake or version check. A server in TCP mode answers `OPEN` (`CLEAR`, `GET`..., `LIST`) and `CHECK` on the asking connection alone, and relays everything else to all clients. A server in file-share mode relays `OPEN` and `CHECK` too, which makes every PC beep and warn, so the relay sends one `OPEN` per connection and no `CHECK` until the server answers. The relay has two roles. As a client it joins an existing N3FJP server, and after the log loads it can send `ADD`, `EDIT` and `DELETE`, but never clock syncs, chat or `BAMS`. As a server, N3FJP PCs connect to it like a local Field Day Commander, and it owns the log and numbering (a deleted number isn't reused, and an empty log starts over at 1, as in N3FJP). Either needs N3FJP's TCP network mode. Neither has been run against real N3FJP software yet.
- **ADIF log files on disk:** read-only and one-way, at most 16 files chosen in the system's file window (`filepicker`, because a browser never tells a page where a file is), and they run beside N3FJP or with its role set to off. Only contacts in the event's dates are queued, because a log can hold years of contacts and the server refuses others for good, so a file waits until a handshake has supplied the dates. Those dates are kept while offline and forgotten on enrolling or disconnecting. Anything but a plain append re-reads the whole file, which is safe because the queue recognizes what it holds. A contact gone from a file is never deleted, since a file can shrink for many reasons. On Windows, open files only through `openShared` and `statFile`, so a logger is never blocked and a file it holds open still shows its real size.
- **N1MM Logger+ broadcasts (UDP 12060):** listen-only, off until someone turns it on, and on loopback until they also open it to other PCs. Edits and deletes don't say the contact's old band and mode, so the key is kept by N1MM's id (`n1mm_records`). N1MM cuts each datagram by one byte per extra UTF-8 byte, so a value that was cut is dropped. The port stays open through every other settings change, because UDP sent to a closed port is gone. Nothing arrives while the relay is closed, and Tools > Rebroadcast all Contest QSO's to other Apps fills in what it can.
- **WSJT-X/JTDX (UDP 2237) and plain ADIF over UDP (2238):** listen-only, both in `udplog`, each off until turned on and on loopback until opened to other PCs. The ports start at the server's defaults and can be changed, but never to one another relay UDP listener uses. WSJT-X sends each contact twice, as QSO Logged (5) and Logged ADIF (12), so only 12 is read. The ADIF listener skips WSJT-X packets. Neither has edits or deletes. Senders put the club call in `STATION_CALLSIGN` on every PC, so a contact from another PC gets a station naming it (`W1AW @ 192.168.1.23`, or `WSJT-X @ …` for a default client id), one from this computer gets none so the pick applies, and a WSJT-X `--rig-name` id is used as it is. Contacts outside the event's dates are skipped once they're known, and queued before then, because a datagram can't wait. Port 2237 is often taken by GridTracker or JTAlert.
- **Other loggers:** the app's listeners in `app/Console/Commands/*ListenCommand.php` show how it handles them.
- **N1MM peer network (12070), if ever joined:** never send `FUNCTIONKEY` (keys another PC's radio), `TIME` (sets its clock) or `FILE` (overwrites its data files).

Implement protocols from their wire format. Never commit third-party source or anything bundled from another vendor, including logos and forms.

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
- **Windows builds are the portable version and the installer,** both from `scripts/build-windows.sh`, as in CI. There's no standalone `relay.exe` to build or hand out, since it only exists inside the installer.

Activate the `branch-workflow` skill before you start, commit or merge work, run Go tooling, or scan with Sonar.

## Writing

- **Commits:** Conventional Commits, describing the change in plain words (`fix(queue): keep unsent contacts after a restart`).
- **Changelog:** anything a user could notice gets a `CHANGELOG.md` entry under **Unreleased** in the same branch. Describe fixes by what the person saw and when, not what changed in the code. Skip refactors, typos and CI. Keep the file to release sections, with no intro.
- **Product name:** Field Day Commander Relay, or "the relay" after the first mention. The server is Field Day Commander. Never "FDC".
- **User-facing prose** (UI text, error and status messages, README, changelog, guides): send it to the `prose-writer` agent, which follows the `docs-prose` skill. Keep the brief to a few lines and point it at the code. Don't draft wording or list what the text must say.
- Only create documentation files when asked.

## Releases

Activate the `release` skill for any version bump, release or tag.
