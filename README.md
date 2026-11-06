# Field Day Commander Relay

Field Day Commander Relay is a desktop program for ham radio clubs using [Field Day Commander](https://github.com/ckoval7/fielddaycommander), the separate server project. It runs on your logging computer, collects contacts from local logging software, and sends them securely to your server, even if it's on another network.

## Project status

This is early software. There isn't an installer, start-at-login support, or a published release yet. Neither N3FJP role has been run against real N3FJP software yet.

The relay needs Field Day Commander's relay support, which isn't in a Field Day Commander release yet. The Relays page and relay API are on the unmerged `feature/relay-api` branch. The Get started steps below won't work against a current Field Day Commander release.

Windows binaries aren't available yet. They will come from [GitHub Actions build artifacts](https://github.com/ckoval7/fielddaycommander-relay/actions/workflows/build.yml) built from `main` and release tags. You need Windows 10 or later. Enrollment requires the Windows keychain. Linux builds compile, but can't enroll yet.

## What it helps with

The relay connects to the server using outbound HTTPS or WSS connections, so it works behind NAT without port forwarding. Contacts are saved locally before sending. While the relay is running, it keeps collecting through internet outages and catches up when the connection returns. Retrying an upload doesn't create duplicate contacts.

## Supported loggers

| Source | Connection | Direction |
| --- | --- | --- |
| N3FJP | TCP network mode. The relay can join an N3FJP server as a client, or act as the server for N3FJP PCs. | Two-way sync of contacts, corrections, and deletes |
| ADIF log files | Files on disk, such as WSJT-X's `wsjtx_log.adi` | Read-only, one-way to the server |
| N1MM Logger+ | Contact broadcasts | One-way to the server, including edits and deletes |

ADIF collection needs an initial server connection to learn the event's dates. After that, it can continue offline. N3FJP file share mode isn't supported. In N3FJP server role, closing the relay stops logging until you start it again.

## Get started

1. Create an enrollment code on the server's **Relays** page.
2. On the relay's status page, enter the **Server address** (HTTPS) and **Enrollment code**, then click **Connect**.
3. Pick your event's **Station** in the connected server card.
4. Set up your logger: choose an N3FJP role under **N3FJP setup**, use **Add log file** for ADIF, or turn on **Receive N1MM contacts** under **N1MM Logger+ setup**. The page has help for each option.

The options **Let other PCs connect** (N3FJP) and **Let other PCs send contacts** (N1MM) open ports to other computers. Neither network interface has a password. Keep them on your club's own network, and never forward those ports from the internet.

## Build from source

Use Go 1.27.3, as specified in `go.mod`. The stylesheet is committed, so a plain Go build doesn't need Node.js. From the repo root, build for Windows with these commands in a POSIX shell:

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui" -o relay.exe ./cmd/relay
go test ./...
```

Keep `CGO_ENABLED=0` for release builds. The tests run for your host platform. `scripts/go.sh` is also available for running Go in a container.

Branch flow: feature branch → `dev` → `main`.

## License

Code is licensed under `GPL-3.0-or-later`. Documentation and images are licensed under `CC-BY-NC-SA-4.0`. The repository follows the REUSE specification, with file coverage recorded in `REUSE.toml` and license texts in `LICENSES/`.
