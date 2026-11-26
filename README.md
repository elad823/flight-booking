# Flight Booking System

A flight booking service in Go, orchestrated with [Temporal](https://temporal.io).
Users pick a flight, hold seats for 15 minutes, and pay with a 5-digit code.
Every seat change restarts the 15-minute timer, payment is retried up to 3
times, and an order that isn't paid in time releases its seats.

## Run it

You need Go 1.27, the [Temporal CLI](https://docs.temporal.io/cli), and, for
`make lint`, golangci-lint v2. On macOS:

```sh
brew install go temporal golangci-lint
```

Then, in two terminals:

```sh
make temporal   # Temporal dev server on localhost:7233, UI on http://localhost:8233
make run        # API server and worker
```

Open http://localhost:8080. The Temporal UI at http://localhost:8233 shows
every flight and order workflow, with its full event history.

### Configuration

| Variable | Used by | Default |
|---|---|---|
| `HTTP_ADDR` | server | `localhost:8080` |
| `TEMPORAL_ADDRESS` | both | `localhost:7233` |
| `TEMPORAL_NAMESPACE` | both | `default` |
| `PAYMENT_FAILURE_RATE` | worker | `0.15` |

Three declines in a row at 15% happen 0.34% of the time, so to see a failed
payment run the worker with a higher rate:

```sh
PAYMENT_FAILURE_RATE=1 go run ./cmd/worker
```

### Make targets

| Target | What it does |
|---|---|
| `make build` | builds `bin/server` and `bin/worker` |
| `make run` | runs the server and the worker |
| `make temporal` | runs the Temporal dev server |
| `make test` | runs the tests |
| `make race` | runs the tests with the race detector |
| `make lint` | runs golangci-lint |
| `make fmt` | formats the code |

## How it works

```
browser ──HTTP──> server ──Temporal client──> Temporal ──tasks──> worker
 (web/)        (cmd/server)                                   (cmd/worker)
                internal/api                                  internal/workflows
                                                              internal/booking
```

- **Server** (`cmd/server`, `internal/api`): the REST API and the embedded web
  page. It holds no state; it starts workflows, sends them Updates, and reads
  them with Queries.
- **Worker** (`cmd/worker`, `internal/workflows`): runs the workflows and
  activities. On start it creates one `FlightWorkflow` per flight, keeping any
  that already run.
- **`FlightWorkflow`**: one per flight, owning its seat map. Temporal runs a
  workflow's code one step at a time, so two orders can't take the same seat,
  and the seat map survives a worker crash.
- **`OrderWorkflow`**: one per order, running its lifecycle:

```
CREATED ──select seats──> SEATS_HELD ──pay──> PAYING ──> CONFIRMED
   │                       │    ↺ change seats            └──> FAILED
   └──── 15 minutes ───────┴──> EXPIRED
```

  Selecting seats holds them on the flight (through an activity that sends the
  flight an Update) and restarts the 15-minute timer. Paying stops the timer.
  Payment gets 10 seconds per attempt, 3 attempts, and one minute in total;
  then the seats are booked and a confirmation is sent (the worker logs it as
  `Booking confirmed`), or the order fails and releases them. If the timer
  runs out, the order expires and releases its seats.
- **Business rules** (`internal/booking`): the seat map, order statuses,
  payment code check, and the payment simulator. Plain Go with no Temporal
  imports, so it is tested with ordinary table tests.

The design decisions, with what was rejected and why, are in
[DOCS/PLAN.md](DOCS/PLAN.md).

## API

| Method and path | Body | Answer |
|---|---|---|
| `GET /api/flights` | | the flight catalog |
| `GET /api/flights/{id}/seats` | | `{"available": [...]}` |
| `POST /api/orders` | `{"flightId": "FB100"}` | 201, the new order |
| `GET /api/orders/{id}` | | the order |
| `PUT /api/orders/{id}/seats` | `{"seats": ["01A", "01B"]}` | the order, with a new `expiresAt` |
| `POST /api/orders/{id}/payment` | `{"code": "12345"}` | 202; poll the order for the result |

An order looks like this:

```json
{"id": "IVBYI7SD4RPOSLKWVF75D47ARZ", "flightId": "FB100", "status": "SEATS_HELD",
 "seats": ["01A", "01B"], "expiresAt": "2026-11-26T21:20:11Z"}
```

Errors come back as `{"error": "..."}` with 400 for a bad request (unknown
seat, malformed code), 404 for an unknown order or a change to a finished
one (reading a finished order still works), 409 when the
current state refuses the request (seat taken, paying twice), and 500 for
anything else, whose details go only to the server log.

A quick session with curl (and `jq`):

```sh
ID=$(curl -s -X POST localhost:8080/api/orders -d '{"flightId":"FB100"}' | jq -r .id)
curl -s -X PUT localhost:8080/api/orders/$ID/seats -d '{"seats":["01A"]}'
curl -s -X POST localhost:8080/api/orders/$ID/payment -d '{"code":"12345"}'
curl -s localhost:8080/api/orders/$ID
```

## Tests

```sh
make race
make lint
```

- `internal/booking`: table tests for the seat map, statuses, and payment.
- `internal/workflows`: Temporal's test suite runs both workflows in simulated
  time, so 15-minute expiries take milliseconds. Cases include timer restarts,
  expiry, a payment at the last second, three declines, a seat lost after
  payment, and overlapping requests.
- `internal/api`, `cmd/server`, `web`: `httptest` tests for routing, input
  checks, and the mapping from workflow errors to status codes.

## Things to know

- **Expected ERROR lines.** When a seat is taken, the worker logs
  `ERROR Activity error ... seat unavailable`. Temporal logs every failed
  activity at ERROR, even a business rejection; the order handles it and the
  user sees a 409.
- **The dev server keeps everything in memory.** Stopping `make temporal`
  forgets all flights and orders; the next worker start creates the flights
  again with every seat free.
- **No login.** An order's ID is 128 random bits and acts as its access key.
- **The countdown uses the browser's clock.** A wrong device clock shifts the
  display; the server's timer is the one that decides.
