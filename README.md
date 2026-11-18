# Cotta

Self-hosted SSH in the browser, without the clutter.

- **One click to connect.** Paste `ssh user@host -p 2222`, a `user@host`, or a bare IP and Cotta fills in the rest.
- **Sessions stay running on the server.** Close the tab, open Cotta on your phone, and pick up where you left off.
- **Saved commands.** Keep commands per server (or for all of them) as buttons under the terminal. Saved commands and anything you paste are shown for review first, and only sent when you press Send.
- **No account, no cloud.** One small container; keys and passwords are encrypted with your master passphrase.

Early MVP: terminal only. File transfer (SFTP) is next.

## Install

On a machine with git and Docker:

```bash
curl -fsSL https://raw.githubusercontent.com/tristenlammi/Cotta/main/install.sh | sh
```

The script prints the address to open: the machine's network address and a port, for example `http://192.168.1.20:8230`. It uses port 8230, or the next free one above it. Choose a master passphrase when it opens; there is no way to recover it.

Cotta installs into `~/cotta` (on Unraid, `/mnt/user/appdata/cotta`) as a container named `Cotta`, reachable from your local network. Settings are in the `.env` file there:

- set `COTTA_BIND=127.0.0.1` to answer on that machine only, or
- set `TUNNEL_TOKEN` (and the `COTTA_CF_*` values) to add a Cloudflare Tunnel for access from outside.

## Update

```bash
cd ~/cotta && ./update.sh
```

This pulls the latest version and restarts Cotta. Run it after editing `.env` too. Restarting ends running sessions; your servers, keys and saved commands are kept.

Your data (the encrypted vault and settings) lives in the `data` folder inside the install folder. Back that folder up.

## Run from source

```bash
go run ./cmd/cotta
```

## Good to know

- SSH connections start from the container, so servers must be reachable from its network.
- Sessions survive closing the browser, not restarting the container.
- Don't expose the port directly to the internet; put Cloudflare Access (or similar) in front.

## Develop

```bash
go test ./...
```

`go run ./cmd/devsshd` starts a fake SSH server on `127.0.0.1:2222` for trying the UI without a real machine.

## Licence

MIT — see [LICENSE](LICENSE).
