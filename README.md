# webterm

Web-based terminal with a file explorer sidebar. Runs on Windows using ConPTY.

## Features

- **Terminal** — full PTY terminal in the browser via WebSocket + [xterm.js](https://xtermjs.org/)
- **File explorer** — sidebar with directory navigation, git status badges (M/A/D/R/?), and current branch display
- **Configuration** — `webterm.ini` file for port and shell settings
- **Single binary** — all static assets embedded via Go `embed`

## Quick start

```
webterm.exe
```

Open `http://localhost:1122` in a browser.

## Configuration

Edit `webterm.ini` next to the executable:

```ini
port = 1122
shell = powershell.exe
```

## Build

Using Make:

```
make build
```

Or directly with Docker Compose:

```
docker compose run --rm build
```

Or natively with Go 1.22+:

```
go build -ldflags="-s -w" -o bin/webterm.exe .
```

## Tech stack

- **Backend**: Go 1.22, [conpty](https://github.com/UserExistsError/conpty), [gorilla/websocket](https://github.com/gorilla/websocket)
- **Frontend**: xterm.js 5, vanilla JS
- **Build**: Docker Compose (golang:1.22-alpine)
