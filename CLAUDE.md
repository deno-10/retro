# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Language

**Always reply to the user in Russian.** This applies to every message, explanation, question and summary, regardless of the language the user writes in. Code identifiers stay in English; comments, error strings, log messages and tests stay in Russian as in the existing code.

## What this is

Retro is a sprint-retrospective board: a single Go binary (the only dependency is the pure-Go `modernc.org/sqlite` driver, no CGO) serving a REST API, Server-Sent Events, and an embedded vanilla-JS frontend. Code comments, log messages, error strings, tests and the README are written in **Russian**; keep new comments, user-facing strings and test failure messages in Russian to match.

## Commands

```bash
go run .                                   # serve on http://localhost:8080, data in data/retro.db (SQLite)
go run . -addr :9000 -data /path/retro.db
go run . -data data/boards.json            # legacy JSON store, chosen by the .json extension
go test ./...                              # all tests
go test ./internal/model -run TestVoteLimit   # a single test
go vet ./...
go build -o retro .
docker build -t retro . && docker run -d -p 8080:8080 -v retro-data:/app/data retro
```

Go 1.26+ is required (`go.mod` pins it because `modernc.org/sqlite` needs it; `GOTOOLCHAIN=auto` downloads it). Routing uses `net/http` method+pattern muxing (`"GET /api/boards/{id}"`, `r.PathValue`) and `http.FileServerFS`. There is no frontend build step; `web/` is served as-is via `//go:embed`, so changes to `web/*` require restarting the Go process (a browser reload alone is not enough).

`data/` and the `retro` binary are gitignored. Tests use `t.TempDir()` and never touch `data/`.

**Before finishing any task, both `go vet ./...` and `go test ./...` must pass.** Run them and fix failures; do not report a task as done while either is red.

## Roadmap

The project roadmap lives in `README.md`, section «Дальше» (Postgres as a new `Store` implementation, OIDC SSO, teams as a first-class entity). Boards already chain via `PrevID`/`NextID`: `Board.NextBoard` carries open action items forward, and `handleNextBoard` reserves `NextID` on the previous board inside `store.Update` before creating the new one, so "only one next" is enforced under the store's lock. Consult it before proposing new features, and update it there rather than duplicating it here.

## Architecture

Four packages with strict dependency direction: `server → store, hub, model`; `store → model`; `model` and `hub` depend on nothing internal.

- **`internal/model`** holds every business rule and no I/O: phases (`write → reveal → vote → discuss`), permissions (owner vs. author), vote limits, templates, input limits. All mutation methods on `Board` take an `actorID`/`participantID` and enforce rights themselves. Sentinel errors (`ErrNotFound`, `ErrForbidden`, `ErrPhase`, `ErrVotes`, `ErrInvalid`) are wrapped with `%w`; the server maps them to 404/403/409/409/400 in `writeErr`. Any new rule or error kind belongs here first.
- **`Board.View(viewer)`** is the only thing the API ever returns to clients. It is where privacy is enforced server-side: in `write` phase other participants' cards come back with `hidden: true` and empty text/author; vote counts appear only from `vote` phase; cards sort by votes in `discuss`. Cards sharing a `groupId` are always emitted contiguously (see `arrange`), the client folds them into one group element, and a vote on any member counts as a vote for the group. Never serialize a raw `Board` to a client.
- **`internal/store`** defines the `Store` interface (`Create`, `Get`, `Update`, `Close`) with two implementations chosen by `store.Open(path)` on file extension. `SQLiteStore` (default, `sqlite.go`) keeps each board as a JSON document in `boards(id, data, created_at, updated_at)`, uses a single pooled connection with WAL and `busy_timeout`, and runs `Update` inside a transaction that rolls back when `fn` errors. `JSONStore` (`store.go`) is the legacy format: in-memory map under a mutex, whole file rewritten atomically on each change. Both return deep copies from `Get`/`Update` and commit only if `fn` returns nil. `ImportJSON` copies a JSON file into any `Store`; `main.go` calls it once when a fresh SQLite DB has a `boards.json` beside it. Keep callers on the interface so Postgres can be added the same way.
- **`internal/hub`** is an in-process pub/sub keyed by board ID that carries no payload. `Notify` is non-blocking and coalesces (buffer of 1). Subscribers re-read the board from the store and rebuild their own `View`. `Hub.Done()`/`Close()` let `main` terminate SSE streams before `http.Server.Shutdown`.
- **`internal/server`** is thin. Every write endpoint goes through `Server.mutate`, which does: resolve participant → `store.Update` → `hub.Notify` → respond with `View`. New mutation endpoints should follow that pattern rather than hand-rolling the sequence. `handleEvents` is the SSE loop: it sends the full `BoardView` plus an `online` count on every signal and a `: ping` comment every 25 s. `withLogging` deliberately does not wrap the `ResponseWriter` because doing so would hide `http.Flusher` and break SSE. Phase timers auto-advance the board server-side: `syncTimer` (called after every mutation and on every board read) keeps an in-memory `time.AfterFunc` per board in step with `TimerEndsAt`, and `fireTimer` applies `Board.ExpireTimer` through `store.Update`, so an expired timer is honoured even after a restart once someone opens the board.

### Participant identity

There is no auth. The browser generates a 32-hex id once, stores it in `localStorage`, and sends it as the `X-Participant` header (or `?participant=` for SSE, since `EventSource` cannot set headers). The server validates it as 8–64 chars of `[A-Za-z0-9-]`. The board's `ownerId` is simply the creator's participant id; "owner" checks everywhere are `actorID == b.OwnerID`.

### Frontend (`web/app.js`)

Single file, no framework. `h(tag, attrs, ...children)` builds DOM nodes (text is always escaped; never use `innerHTML`). The board page re-renders columns entirely on every SSE `board` event; inputs that must survive a re-render carry a `data-keep` attribute and `captureFocus`/`restoreFocus` preserve value, focus and caret across the swap. Routing is a single regex on `location.pathname` (`/` → home, `/b/{id}` → board); the server serves `index.html` for both. Mutations are fire-and-forget `api()` calls whose response is ignored because the SSE event delivers the new state; errors surface via `toast()`.

## Testing conventions

Tests live beside the code as `*_test.go`. `server_test.go` builds a full handler with `store.OpenJSON(t.TempDir())`, `hub.New()`, an `fstest.MapFS` for static files and a discarded `slog` logger, then drives it with `httptest` and raw `map[string]any` JSON assertions. Model tests use a `newTestBoard` helper with owner id `owner-1`. Participant ids in server tests must be ≥ 8 characters or the request is rejected with 400.
