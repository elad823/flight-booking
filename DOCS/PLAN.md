# Implementation Plan

Tracks the agreed plan for the Flight Booking System. Requirements: `DOCS/requirements.md`.

## Decisions

### 1. Seat inventory: one entity workflow per flight (`FlightWorkflow`)

- **Why:** The inventory survives a worker crash, and Temporal runs each workflow one step at a time, so two orders can't take the same seat without a mutex. Different flights still run in parallel. The spec lists entity workflows as an accepted approach.
- **Cost:** Workflows can't call each other synchronously, so orders reach the flight through an activity that sends an Update (with a stable Update ID to avoid double-applying on retry). Flight workflows need starting, and need Continue-As-New to keep history bounded. Holds also need their own expiry in the flight workflow in case an order is killed abnormally.
- **Rejected:** An in-memory map with `sync.Mutex` loses all holds on restart and works with only one worker. SQLite adds a dependency and a DB layer the spec doesn't need.

### 2. "3 retry attempts": Temporal retry policy on the payment activity

- **Choice:** 3 attempts, 10 s `StartToCloseTimeout` per attempt. A malformed code is a non-retryable error. The failure rate comes from an env var, default 0.15.
- **Why:** It matches the spec ("retry up to 3 times, then the order fails") and its list of rules "implemented by Temporal". The retry behavior lives in a retry policy, not hand-written loop code, and the order state machine stays linear.
- **Why the env var:** 3 failures in a row at 15% happen only 0.34% of the time, so the failure path is hard to demo without raising the rate.
- **Rejected:** Letting the user re-enter the code up to 3 times needs extra states (a declined loop), extra UI, and our own retry counting.

### 3. Web app: one `index.html` with plain JS, polling

- **Choice:** Embedded in the server binary with `go:embed`. It polls order status, and the countdown runs in the browser from `expiresAt`.
- **Why:** No build tools or framework, and a single binary to run. Because the countdown is computed locally, polling only has to catch status changes.
- **Rejected:** SSE adds a goroutine per connection and connection cleanup for little gain at this size. No UI at all would skip the Web App component in the spec.

### 4. API to workflow: Workflow Updates for changes, Query for status

- **Why:** An Update returns an answer, so the API can report "seat taken" or "invalid code" immediately. A Query reads state without changing it.
- **Rejected:** A signal returns nothing, so the client would have to poll to find out a request was rejected.

### 5. Tooling: golangci-lint 2.15.0 and temporal CLI 1.10.1, installed with Homebrew

- **Why:** The lint config is written for the installed golangci-lint version (v2 format). The temporal CLI provides `temporal server start-dev` for local runs without Docker.

### 6. Flight workflows start eagerly at startup

- **Why:** Every flight exists before any order, so listing available seats always works. Starting uses `WorkflowIDConflictPolicy: USE_EXISTING`, so a restart reuses running flights.
- **Rejected:** Update-with-Start on the first hold. Reads would have to handle a flight workflow that doesn't exist yet, and the reservation activity would need each flight's seat list.

### 7. Flight Updates use a validator and a handler

- **Why:** A rejected Update (such as "seat taken") leaves nothing in history, so busy flights use less of the 2,000-Update budget. The validator calls the read-only `CheckHold` / `CheckConfirm`, and the handler re-checks inside `Hold` / `Confirm`.
- **Rejected:** Handler only. Every rejection would be recorded in history.

### 8. Continue-As-New follows the server's suggestion

- **Why:** `GetContinueAsNewSuggested()` covers history size, event count, and 90% of the Update limit, so we track nothing ourselves. The workflow waits for `AllHandlersFinished` first so no Update is cut off. Tested with the testsuite's `SetContinueAsNewSuggested(true)`.
- **Rejected:** Our own Update counter. It duplicates what the server tracks and can drift from the real limits.

### 9. The pay Update returns once accepted

- **Why:** Payment can take up to a minute with retries. The handler moves the order to PAYING and returns at once; the UI already polls status and sees the result there. HTTP requests stay short.
- **Rejected:** Waiting for the result in the Update, which would hold the HTTP request open for the whole payment.

### 10. Order handlers take a workflow.Mutex

- **Why:** The seat-change handler waits on an activity, so without a lock a second request could run mid-way and leave the order and the flight disagreeing about the seats. Every handler re-checks the status after taking the lock, because the validator ran before the wait.
- **Note:** `workflow.Mutex` guarantees one request at a time, not arrival order (its Lock is an Await on "unlocked"). The re-check keeps every order of execution correct.
- **Rejected:** A busy flag that rejects requests while a change is in progress; the UI would have to retry.

### 11. Clicking pay stops the timer; payment confirms or fails the order

- **Choice:** The 15-minute timer stops when payment starts and never resumes, because payment either confirms the order or fails it (after 3 attempts, as the spec says). Payment gets 10 seconds per attempt, 3 attempts, and one minute in total from the click (`ScheduleToCloseTimeout`).
- **Why:** A user who pays in the last second still gets a full payment. The one-minute cap stops a stuck payment from holding the order forever.
- **Flight backstop:** the flight holds seats until the order's deadline plus 2 minutes: one for a last-second payment and one for confirming the seats.
- **Rejected:** Resuming the timer after a failed payment so the user can pay again. It would change the spec's rule that the order fails after 3 failures.

Assumptions:
- Flights are a fixed list in code (for example 3 flights with 30 seats each).
- An order with no seats selected closes after 15 minutes.
- A payment submitted before the hold expires is accepted, even if processing runs past the expiry.

## Directory structure

```
cmd/server/       REST API + web UI
cmd/worker/       Temporal worker
internal/booking/ pure domain: types, statuses, seat map rules, payment code check
internal/workflows/ OrderWorkflow, FlightWorkflow, activities
internal/api/     HTTP handlers and routing
web/              embedded index.html
```

## Steps

Each step is approved before the next one starts.

| # | Step | Tests | Status |
|---|---|---|---|
| 1 | `git init`, `go mod init`, `.gitignore` | | Done |
| 2 | `.golangci.yml` (v2) | | Done |
| 3 | `Makefile` (build, run, test, race, lint, fmt) | | Done |
| 4 | `internal/booking`: types, statuses, errors | table tests | Done |
| 5 | `internal/booking`: seat map (hold, release, confirm, conflicts) | table tests | Done |
| 6 | `internal/booking`: payment code check and failure simulator | table tests | Done |
| 7 | `FlightWorkflow` (7a: `CheckHold` / `CheckConfirm`; 7b: workflow) | testsuite | Done |
| 8 | Activities: inventory, payment, confirmation | unit tests | Done |
| 9 | `OrderWorkflow` | testsuite: timer reset, expiry, retries, failure | Done |
| 10 | `cmd/worker` | | |
| 11 | `internal/api`: handlers behind a client interface | `httptest` | |
| 12 | `cmd/server` with graceful shutdown | | |
| 13 | `web/index.html` | | |
| 14 | README and end-to-end run against `temporal server start-dev` (include: a lapsed hold shows as available in the `available` Query on a real server) | `make race`, `make lint` | |
| 15 | Scaling hardening: 2 s poll interval, idempotency key on payment, "Scaling notes" in README | | |

## Scaling notes (for Step 15)

- `temporal server start-dev` is for development only; production needs a Temporal cluster or Temporal Cloud.
- Every status poll is a Query served by a worker; at large scale, poll less often, push with SSE, or serve reads from a separate store.
- One very popular flight serializes all its requests in one workflow; Temporal also limits each workflow run to 10 in-flight and 2,000 total Updates by default; past 10 in flight, callers get `ResourceExhausted`. Fix by sharding a flight into sections.
- Activities run at least once, so a real payment call needs an idempotency key to avoid double charges.
