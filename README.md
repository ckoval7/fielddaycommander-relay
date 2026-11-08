# Field Day Commander Relay

Field Day Commander Relay is a desktop program for ham radio clubs using [Field Day Commander](https://github.com/ckoval7/fielddaycommander), the separate server project. It runs on your logging computer, collects contacts from local logging software, and sends them securely to your server, even if it's on another network.

## Project status

This is early software. There isn't a published release yet. Neither N3FJP role has been run against real N3FJP software yet.

The relay needs Field Day Commander's relay support, which isn't in a Field Day Commander release yet. The Relays page and relay API are on the unmerged `feature/relay-api` branch. The Get started steps below won't work against a current Field Day Commander release.

[GitHub Actions build artifacts](https://github.com/ckoval7/fielddaycommander-relay/actions/workflows/build.yml) from `main` and release tags include both `relay-setup.exe` and `relay-portable.exe` for Windows. You need 64-bit Windows 10 or later. Enrollment requires the Windows keychain. Linux builds compile, but can't enroll yet.

## What it helps with

The relay connects to the server using outbound HTTPS or WSS connections, so it works behind NAT without port forwarding. Contacts are saved locally before sending. While the relay is running, it keeps collecting through internet outages and catches up when the connection returns. Retrying an upload doesn't create duplicate contacts.

## Supported loggers

| Source | Connection | Direction |
| --- | --- | --- |
| N3FJP | TCP network mode. The relay can join an N3FJP server as a client, or act as the server for N3FJP PCs. | Two-way sync of contacts, corrections, and deletes |
| ADIF log files | Files on disk, such as WSJT-X's `wsjtx_log.adi` | Read-only, one-way to the server |
| N1MM Logger+ | Contact broadcasts | One-way to the server, including edits and deletes |

ADIF collection needs an initial server connection to learn the event's dates. After that, it can continue offline. N3FJP file share mode isn't supported. In N3FJP server role, closing the relay stops logging until you start it again.

## Install on Windows

Choose `relay-setup.exe` for your club's regular logging PC, or `relay-portable.exe` for a borrowed PC or one where you can't or don't want to install anything. Download either from this project's GitHub build artifacts. Neither version is code signed yet, so Windows SmartScreen may warn. To go ahead, click **More info**, then **Run anyway**, only for a file from this project's GitHub build.

Run `relay-setup.exe` to install.

It installs for your Windows account in `AppData\Local\Programs`, without administrator rights or a folder choice. It adds a Start menu shortcut and an entry in Windows' Apps list. On the last page, **Start the relay now** is checked. **Start the relay when I sign in to Windows** is unchecked, or hidden if this install already starts at sign-in.

To update, run the new installer. It asks before closing a running relay cleanly. Saved contacts and your server connection are kept. Contacts your loggers send while it's closed may need sending again. N1MM broadcasts are lost while it's closed.

Uninstall from Windows' Apps list. This removes the program, shortcut, and this install's start-at-sign-in entry. Collected contacts, settings, and the connection token stay on the computer. To remove the token too, click **Disconnect** on the status page before uninstalling. The installer doesn't open firewall ports. Windows may still ask for permission when you turn on network options.

For the portable version, put `relay-portable.exe` in a folder you can write to and run it. It needs no install and doesn't add a Start menu shortcut or Apps list entry. It doesn't offer start at sign-in, and the status page hides that option.

Contacts, settings, and logs are saved in `relay-data` beside the program. Move or back up the program together with that folder. If the folder can't be written, the relay uses the installed version's usual data folder in AppData so it can still collect contacts, and notes this in `relay.log`.

The connection token stays in that PC's Windows keychain. The relay never writes it to a file, so it doesn't travel with the folder. On a different PC or Windows account, the status page asks for a new enrollment code. Queued contacts are kept and go out after connecting. Each portable folder has its own token, separate from the installed version's and other portable folders' tokens on the PC. To remove the token from a PC, click **Disconnect** before removing the program.

To remove the portable version, check that **Waiting** is zero, then delete the program and `relay-data`. Unsent contacts are in that folder.

Only one relay can run per computer, whichever version, because logger ports belong to the whole computer. Starting it again opens the running relay's status page, and the new copy stops. If another Windows account's relay is running on this PC, yours won't run. A portable copy and an installed copy don't share contacts or settings, except when the portable copy falls back to the usual AppData folder.

## Get started

1. Create an enrollment code on the server's **Relays** page.
2. On the relay's status page, enter the **Server address** (HTTPS) and **Enrollment code**, then click **Connect**.
3. Pick your event's **Station** in the connected server card.
4. Set up your logger: choose an N3FJP role under **N3FJP setup**, use **Add log file** for ADIF, or turn on **Receive N1MM contacts** under **N1MM Logger+ setup**. The page has help for each option.

The options **Let other PCs connect** (N3FJP) and **Let other PCs send contacts** (N1MM) open ports to other computers. Neither network interface has a password. Keep them on your club's own network, and never forward those ports from the internet.

## Build from source

To build Field Day Commander Relay for Windows, run these commands from the repo root in a POSIX shell with Docker available:

```sh
scripts/build-windows.sh
scripts/go.sh test ./...
```

The build creates `dist/relay-portable.exe` (the portable version) and `dist/relay-setup.exe` (the installer). Go runs in a container.

The installer build uses NSIS's `makensis` if installed, or a container otherwise.

Branch flow: feature branch → `dev` → `main`.

## License

Code is licensed under `GPL-3.0-or-later`. Documentation and images are licensed under `CC-BY-NC-SA-4.0`. The repository follows the REUSE specification, with file coverage recorded in `REUSE.toml` and license texts in `LICENSES/`.
