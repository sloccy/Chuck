# Chuck

A self-hosted, browser-based file transfer tool. Send files directly between browsers with no disk I/O on the server — chunks stream through via WebSocket passthrough using `NextWriter` + `io.Copy`.

## Features

- **Zero server-side storage** — binary data is never written to disk or held in RAM on the server
- **Auth-gated sending** — only registered users can create transfer links
- **Public receiving** — anyone with the link can receive (no account needed)
- **Folder support** — folders and multi-file selections are zipped in-browser via JSZip before sending
- **Progress + speed** — real-time transfer progress and throughput on both sender and receiver
- **Admin panel** — web UI to add/remove users
- **Single binary** — templates and static files are embedded; no external dependencies at runtime

## Quick Start (Docker)

```yaml
services:
  chuck:
    image: ghcr.io/sloccy/chuck:latest
    ports:
      - "8080:8080"
    volumes:
      - chuck_data:/data
    environment:
      ADMIN_EMAIL: admin@localhost
      ADMIN_PASSWORD: changeme
      DATA_DIR: /data

volumes:
  chuck_data:
```

```bash
docker compose up -d
```

Then visit `http://localhost:8080`.

Default admin credentials (change via env):
- Email: `admin@localhost`
- Password: `changeme`

## Configuration

All config is via environment variables:

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `ADMIN_EMAIL` | `admin@localhost` | Admin login email |
| `ADMIN_PASSWORD` | `changeme` | Admin login password |
| `PUBLIC_BASE_URL` | *(auto-detected)* | Full URL prefix for generated links (e.g. `https://chuck.example.com`) |
| `DATA_DIR` | `.` | Directory for `users.json` |
| `MAX_CONCURRENT` | `0` (unlimited) | Max simultaneous active transfers |

## Usage

1. **Admin setup:** Log into `/admin/login` → add user accounts
2. **Send a file:**
   - Log in at `/login`
   - Drop files or use the file/folder pickers
   - Click **Generate Transfer Link**
   - Copy and share the link
3. **Receive a file:**
   - Open the transfer link in any browser
   - Download starts automatically when the sender begins the transfer

## Local Development

```bash
# Download real JSZip (the placeholder won't work for multi-file/folder transfers)
curl -o static/jszip.min.js \
  https://cdnjs.cloudflare.com/ajax/libs/jszip/3.10.1/jszip.min.js

go run .
```

## Architecture

```
Browser (sender)          Server                    Browser (receiver)
     |                      |                              |
     |-- WS /ws/dashboard --+-- WS /ws/transfer ----------|
     |                      |                              |
     | [binary chunk] ----->| NextReader()                 |
     |                      | NextWriter() + io.Copy() --->| [binary chunk]
     |                      |                              |
```

The server holds one goroutine per WebSocket connection. For binary messages it calls `conn.NextReader()` to get an `io.Reader`, then immediately pipes it to the receiver via `conn.NextWriter()` + `io.Copy()`. No intermediate buffer, no disk I/O.

## Reverse Proxy (nginx example)

```nginx
location / {
    proxy_pass http://localhost:8080;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 3600s;
}
```
