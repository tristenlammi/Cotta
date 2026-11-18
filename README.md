# Cotta

Self-hosted SSH in the browser, without the clutter.

- **One click to connect.** Paste `ssh user@host -p 2222`, a `user@host`, or a bare IP and Cotta fills in the rest.
- **Sessions stay running on the server.** Close the tab, open Cotta on your phone, and pick up where you left off.
- **No account, no cloud.** One small container; keys and passwords are encrypted with your master passphrase.

Early MVP: terminal only. File transfer (SFTP) is next.

## Run

```bash
go run ./cmd/cotta
```

Open http://localhost:8080 and choose a master passphrase. There is no way to recover it.

## Docker

```bash
cp .env.example .env
docker compose --profile tunnel up -d --build
```

This runs Cotta behind a Cloudflare Tunnel with no port published; fill in `.env` first. To try it locally instead, uncomment the `ports` lines in `docker-compose.yml` and drop `--profile tunnel`.

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
