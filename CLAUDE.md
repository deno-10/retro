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
npm ci && npx playwright install chromium
npm test                                  # browser regressions, Node.js 20+
npm run test:integration                  # real Go server + temporary SQLite + browser
python3 -m unittest discover -s tests/ops -v # backup/monitor failure regressions
go build -o retro .
docker build -t retro . && docker run -d -p 8080:8080 -v retro-data:/app/data retro
```

Go 1.26+ is required (`go.mod` pins it because `modernc.org/sqlite` needs it; `GOTOOLCHAIN=auto` downloads it). Routing uses `net/http` method+pattern muxing (`"GET /api/boards/{id}"`, `r.PathValue`) and `http.FileServerFS`. There is no frontend build step; `web/` is served as-is via `//go:embed`, so changes to `web/*` require restarting the Go process (a browser reload alone is not enough).

`data/` and the `retro` binary are gitignored. Tests use `t.TempDir()` and never touch `data/`.

**Before finishing any task, both `go vet ./...` and `go test ./...` must pass.** Run them and fix failures; do not report a task as done while either is red.

## Roadmap

The project roadmap lives in `README.md`, section «Дальше» (Postgres as a new `Store` implementation, OIDC SSO, teams as a first-class entity). Boards already chain via `PrevID`/`NextID`: `Board.NextBoard` carries open action items forward, and `store.CreateNext` saves the new board and the previous board's `NextID` atomically, so concurrent requests and storage failures cannot leave a dangling link. Consult the roadmap before proposing new features, and update it there rather than duplicating it here.

## Architecture

Four packages with strict dependency direction: `server → store, hub, model`; `store → model`; `model` and `hub` depend on nothing internal.

- **`internal/model`** holds every business rule and no I/O: phases (`write → reveal → vote → discuss`), permissions (owner vs. author), vote limits, templates, input limits. All mutation methods on `Board` take an `actorID`/`participantID` and enforce rights themselves. Sentinel errors (`ErrNotFound`, `ErrForbidden`, `ErrPhase`, `ErrVotes`, `ErrInvalid`) are wrapped with `%w`; the server maps them to 404/403/409/409/400 in `writeErr`. Any new rule or error kind belongs here first.
- **`Board.View(viewer)`** is the only thing the API ever returns to clients. It is where privacy is enforced server-side: in `write` phase other participants' cards come back with `hidden: true` and empty text/author; vote counts appear only from `vote` phase; cards sort by votes in `discuss`. Cards sharing a `groupId` are always emitted contiguously (see `arrange`), the client folds them into one group element, and a vote on any member counts as a vote for the group. Never serialize a raw `Board` to a client.
- **`internal/store`** defines the `Store` interface (`Create`, `CreateMany`, `CreateNext`, `Get`, `Update`, `Close`) with two implementations chosen by `store.Open(path)` on file extension. `SQLiteStore` (default, `sqlite.go`) keeps each board as a JSON document in `boards(id, data, created_at, updated_at)`, uses a single pooled connection with WAL and `busy_timeout`, and runs mutations inside transactions. `JSONStore` (`store.go`) is the legacy format: in-memory map under a mutex, whole file rewritten atomically on each change. Both return detached boards and roll back mutations on errors. `CreateNext` saves both sides of the board chain atomically. `CreateMany` is an all-or-nothing batch used by `ImportJSON`; `main.go` imports a neighboring `boards.json` when SQLite is empty, so a failed import can be retried on restart. Keep callers on the interface so Postgres can be added the same way.
- **`internal/hub`** is an in-process pub/sub keyed by board ID that carries no payload. `Notify` is non-blocking and coalesces (buffer of 1). Subscribers re-read the board from the store and rebuild their own `View`. `Hub.Done()`/`Close()` let `main` terminate SSE streams before `http.Server.Shutdown`.
- **`internal/server`** is thin. Single-board mutation endpoints go through `Server.mutate`, which does: resolve participant → `store.Update` → `hub.Notify` → respond with `View`. New mutation endpoints should follow that pattern rather than hand-rolling the sequence. `handleEvents` is the SSE loop: it sends the full `BoardView` plus an `online` count on every signal and a `: ping` comment every 25 s. `withLogging` records status/bytes through a wrapper that preserves `http.Flusher`, `FlushError` and `Unwrap`; keep these interfaces working for SSE and write deadlines. Phase timers auto-advance the board server-side: `syncTimer` re-reads the current board under the scheduler mutex before changing a timer. Timer callbacks verify their generation and apply `Board.ExpireTimer` through `store.Update`; stale snapshots or callbacks cannot cancel newer timers. An expired timer is honoured even after a restart once someone opens the board. Never acquire the scheduler mutex inside a store callback.

### Participant identity

The app has no per-user authentication. Production Compose requires shared Basic authentication at Caddy over HTTPS; it never publishes the backend port. This closes access to the team, not to individual boards within the team. The browser pins a 32-hex id in `sessionStorage` for the tab, reusing a valid `localStorage` id on the first visit or generating one. Writes must be verified by reading them back. A valid session id wins on reload; never overwrite a different valid local id or treat a failed read as an empty store. Show a persistent warning if only session persistence works, or if neither store retains the id. The browser sends it as the `X-Participant` header (or `?participant=` for SSE, since `EventSource` cannot set headers). The server validates it as 8–64 chars of `[A-Za-z0-9-]`. The board's `ownerId` is simply the creator's participant id; "owner" checks everywhere are `actorID == b.OwnerID`. The local admin CLI can transfer ownership after storage loss; there is no HTTP ownership recovery endpoint.

### Frontend (`web/app.js`)

Single file, no framework. `h(tag, attrs, ...children)` builds DOM nodes (text is always escaped; never use `innerHTML`). The board page re-renders columns when it applies a board snapshot. All editor drafts live outside the DOM; `data-keep` and `captureFocus`/`restoreFocus` preserve focus and caret across the swap, including when another editor has focus. Failed submissions retain their payload separately from the current draft for retry. Routing is a single regex on `location.pathname` (`/` → home, `/b/{id}` → board); the server serves `index.html` for both. Board mutations use `mutate()` to apply their HTTP responses; SSE and fallback GET responses use the same `applyBoard()` revision check. Errors surface via `toast()` or the editor's conflict controls, and failed creations remain available in the recovery panel.

## Testing conventions

Tests live beside the code as `*_test.go`. `server_test.go` builds a full handler with `store.OpenJSON(t.TempDir())`, `hub.New()`, an `fstest.MapFS` for static files and a discarded `slog` logger, then drives it with `httptest` and raw `map[string]any` JSON assertions. Model tests use a `newTestBoard` helper with owner id `owner-1`. Participant ids in server tests must be ≥ 8 characters or the request is rejected with 400.

Browser regressions live in `tests/frontend/` and use Node’s test runner with Playwright. Run `npm test` after frontend changes. They load the real web assets with controlled HTTP/SSE responses to check draft preservation and delayed request failures.

Live integration tests in `tests/integration/` build and start the Go binary on a local port with a temporary SQLite database. Run `npm run test:integration` after changes to the API/client contract. They check concurrent editing, HTTP updates without SSE, lost responses with safe retries, and action completion against the actual backend. Back/Forward regressions enable Chromium BFCache and assert both `pageshow.persisted` and the original document identity; synthetic events alone do not cover navigation. Tests stop their server and remove temporary data.

Live integration uses the full Chromium channel (`channel: 'chromium'`) in headless mode, or an explicit `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH`. Headless Shell does not restore the native SSE scenario from BFCache. Install full Chromium (`playwright install chromium`, not `--only-shell`) and keep the strict document identity/persisted assertions.

Reload and timeout regressions check both draft persistence and server outcomes. Browser clocks may advance request deadlines, but integration requests still use the real Go handler and SQLite. Never use the application's regular `data/` for these tests.

### Revisions and retries

`Board.Revision` orders all board snapshots. Every successful `Store.Update` sets it to the stored revision plus one, and `CreateNext` increments the previous board's revision atomically with the link. Card and action revisions protect edits: `EditCard`/`EditAction` require the expected entity revision and return `ErrConflict` (HTTP 409) when stale. HTTP PATCH requires `revision`; legacy records without that field have revision 0. `SetActionDone` takes an explicit boolean, so repeating `done: true` does not toggle it back.

`SetVote(cardID, participantID, voted)` also takes an explicit desired state; HTTP `/vote` requires `voted`. Repeating `true` must not remove a vote or spend another vote. `false` clears the participant's votes across the card's current group, including votes carried into it by grouping previously voted cards. Other participants' votes stay intact.

POST cards/actions accept `Idempotency-Key`. The browser creates one key for a submission and keeps it for every retry. The server stores a receipt in `Board.Receipts` in the same store transaction as the mutation. Keys are scoped to board and participant; a matching operation and payload are replayed without another create, even after deletion or restart. A different operation or payload with the same key is a conflict. Receipts must never appear in `BoardView`. Requests without a key keep the older API behavior without duplicate protection.

The browser applies HTTP replies, fallback GET replies and SSE events through a shared revision check. Older board snapshots must not replace newer ones; equal-revision SSE events may still update the online count. Editor drafts keep their original entity revision across incoming updates. Conflict responses preserve the draft and require an explicit choice before replacing server content. SSE errors enable background GET polling until board events resume.

Drafts and the creation outbox persist in a versioned `sessionStorage` snapshot scoped to board and participant. Save the creation payload and idempotency key before sending; pending creations become manual retries after reload. Never restore transient pending flags or automatically rebase editor revisions. Clear saved state only after confirmed success or an explicit user action. Storage errors must produce a visible warning without blocking in-memory editing. API requests have a 15-second deadline covering fetch and response-body reading; uncertain PATCH outcomes preserve the draft and require a fresh read and explicit conflict resolution before another save.

On `pagehide`, persist drafts, invalidate the request generation, abort outstanding requests and close the stream. On BFCache `pageshow`, restore the latest tab snapshot before reconnecting. Guard all asynchronous state changes, including catch/finally cleanup, with the generation. Reset transient pending state and avoid restoring stale DOM input values over the reloaded drafts. Failed storage writes must not cause in-memory drafts to be replaced silently; preserve both versions on divergence and require explicit confirmation before discarding unsaved text.

`Server.mutate` checks timer expiry inside `Store.Update`, after acquiring the store transaction/lock and before checking the requested operation's phase. On expiry, run the operation on a detached candidate so a rejected operation cannot leak partial mutations: commit the phase transition alone, then return the operation error. Storage failure rolls back both. Notify and synchronize scheduling only outside the store callback. An idempotency replay without expiry still must not increment the board revision.

Decode and validate request structure before calling `mutate`: require exactly one JSON value followed by EOF, allowing whitespace within the 1 MiB total body limit. Trailing JSON/garbage and malformed input must not mutate state or notify subscribers. Timer `minutes` is a required integer pointer at the HTTP boundary; missing/null is HTTP 400, explicit zero stops the timer. Keep numeric range rules in the model.

### Operations

`deploy/README.md` is the runbook. `compose.yaml` deploys one backend and Caddy with persistent volumes, resource limits, log rotation and `unless-stopped`. Do not expose port 8080 in production. Only the exact trusted proxy CIDR may supply X-Real-IP/X-Forwarded-Proto; rate limiting must not use caller-chosen participant IDs. Cross-origin mutations are forbidden, including a different origin scheme.

`internal/server/operations.go` provides liveness, database readiness, bounded traffic guards and Prometheus text metrics without per-board/user labels. Logging wraps ResponseWriter while preserving Flusher, FlushError and Unwrap. Preserve per-write SSE deadlines and clear them after each successful flush; a short global WriteTimeout would break idle streams. SSE slot accounting is atomic and must release on every exit. Limits apply to `/next` as well as initial creation.

Store quotas (board count and serialized size) are checked inside the same transaction/lock as writes. A rejected batch or next retro must leave no partial state. Receipts are capped without eviction: old retries still succeed at capacity; never expire a receipt and silently create a duplicate.

CLI `backup` uses VACUUM INTO, verifies the snapshot and publishes only to a new path; `restore` similarly refuses existing DB/WAL/SHM files. Never snapshot a live WAL database by copying only the .db file. `deploy/ops.py` uploads with restic, restores the exact new snapshot and verifies it before advancing the success timestamp. Tests must never use normal application data or real notification recipients.

Run `python3 -m unittest discover -s tests/ops -v` for backup/monitor code. `tests/deployment` needs RETRO_TEST_BINARY, CADDY_TEST_BINARY and restic in PATH; it uses temporary native processes, local TLS and data. `container_smoke.py` is an explicit Docker/CI test and uses temporary project/volume names. Full commands are in the runbook. Secrets live outside Git/build context. No production domain, backup credentials or webhook should be committed.
