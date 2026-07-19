# Transactional Reservation Service

A compact backend for scarce-inventory reservations. It focuses on the parts a
CRUD demo usually skips: concurrent correctness, idempotency, expiring holds,
durable state transitions, and reproducible load measurements.

## What it demonstrates

- Atomic inventory updates under concurrent requests
- Idempotency-key replay and conflict detection
- TTL-based holds with inventory restoration
- Explicit checkout/release state transitions
- Crash-safe state persistence through fsync + atomic rename
- Domain invariants checked in tests and benchmarks
- Prometheus-compatible service metrics

```mermaid
flowchart LR
    Client -->|HTTP + Idempotency-Key| API
    API --> Store[Transactional Store]
    Store --> State[Atomic JSON Snapshot]
    Store --> Events[Append-only Domain Events]
    Reaper[TTL Reaper] --> Store
    API --> Metrics[/metrics]
```

The store serializes each mutation, applies it to an isolated state copy,
verifies inventory-conservation invariants, persists the copy with `fsync` and
an atomic rename, and only then publishes it to readers. This is intentionally
simple enough to audit while retaining the correctness properties expected of
a reservation system.

## Run

```bash
make test
make run
```

Create inventory and reserve one unit:

```bash
curl -sS -X POST localhost:8080/v1/resources \
  -d '{"id":"class-101","capacity":20}'

curl -sS -X POST localhost:8080/v1/resources/class-101/holds \
  -H 'Idempotency-Key: checkout-42' \
  -d '{"quantity":1,"ttl_ms":30000}'
```

The second identical request returns the same hold with
`Idempotent-Replay: true`. Reusing the key with different parameters returns
`409 Conflict`.

## Benchmark

```bash
make benchmark
```

The benchmark performs hold-and-checkout operations through concurrent
workers, reports throughput and latency percentiles, and fails its correctness
signal if the final inventory invariants do not hold. Results depend on the
machine. A reproducible 10,000-request run is checked into
[`benchmarks/darwin-arm64.json`](benchmarks/darwin-arm64.json); it completed
with zero request failures and zero inventory-invariant violations.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/v1/resources` | Create scarce inventory |
| `GET` | `/v1/resources/{id}` | Read remaining inventory |
| `POST` | `/v1/resources/{id}/holds` | Create an idempotent TTL hold |
| `GET` | `/v1/holds/{id}` | Read hold state |
| `POST` | `/v1/holds/{id}/checkout` | Commit a held reservation |
| `DELETE` | `/v1/holds/{id}` | Release a held reservation |
| `GET` | `/v1/events?after=N` | Read domain events |
| `GET` | `/metrics` | Read Prometheus text metrics |

## Design tradeoffs

The serialized state copy is optimized for correctness and inspectability, not
large datasets. A production evolution would place the same domain invariants
behind PostgreSQL row locks and a transactional outbox, then partition by
resource ID. The current implementation is fully runnable without external
services and includes a persistence/restart test.

## License

MIT
