# Phase 2 Handover — Adapter Layer

Phase 2 delivers the real-time market-data pipeline connecting Polymarket and Kalshi
into a unified order-book view with arbitrage detection, persistence, and safety gating.
All 27 tests across 3 packages pass. The layer is ready for the Phase 3 Execution Engine.

---

## 1. Component Map

| Component | File | Role |
|---|---|---|
| **WSClient** | `internal/adapter/websocket.go` | Low-level WebSocket transport. Manages connection lifecycle, exponential-backoff reconnect (50 ms → 5 s, 2×), 500 ms heartbeat timeout, and fan-out of raw `[]byte` frames to subscribers. Exposes an atomic `CircuitState` (Closed / Open) consumed by the CircuitBreaker. |
| **PolyAdapter** | `internal/adapter/poly/adapter.go` | Polymarket-specific parser. Subscribes to WSClient, decodes `book` JSON events (string price/size → `float64`), and emits `BookUpdate` values. Uses `sync.Pool` for `PriceLevel` slice reuse. |
| **KalshiAdapter** | `internal/adapter/kalshi/adapter.go` | Kalshi-specific parser. Performs RSA-PSS auth (`KALSHI-ACCESS-*` headers), handles `orderbook_snapshot` + `orderbook_delta` messages, maintains internal book state per market, normalises cents → 0-1 range, and emits `BookUpdate`. |
| **Broadcaster** | `internal/adapter/broadcaster.go` | Central fan-out hub. Adapters register via `UpdatesProvider`. Consumers call `Subscribe(exchange, marketID)` for filtered streams or `SubscribeAll()` for the unified feed. One goroutine per source; non-blocking dispatch. |
| **RedisWriter** | `internal/adapter/redis_writer.go` | Persistence layer. Reads the `SubscribeAll()` feed, extracts best bid/ask, and writes to Redis. Duplicate suppression skips writes when prices haven't changed. Two-goroutine pipeline (ingest → flush) with a 1024-slot internal buffer. |
| **UnifiedBook** | `internal/adapter/unified_book.go` | Cross-exchange arbitrage detector. Pairs a Polymarket market with a Kalshi market. Emits `ArbitrageEvent` when spread exceeds a configurable threshold. |
| **CircuitBreaker** | `internal/adapter/circuit_breaker.go` | Safety gate. `CanTrade(exchange, marketID)` must return `true` before any order is sent. Checks four conditions (see §4). |
| **TunnelManager** | `internal/adapter/tunnel.go` | Per-user private WS sessions keyed by `(UserID, Exchange)`. Each user gets a dedicated `WSClient`; messages never cross users. Credentials held in-memory only. |

---

## 2. Data Schema

### BookUpdate (internal/adapter/types.go)

```go
type BookUpdate struct {
    Exchange  Exchange     // "polymarket" | "kalshi"
    MarketID  string       // exchange-native market identifier
    AssetID   string       // specific token/asset
    Bids      []PriceLevel // sorted bid levels
    Asks      []PriceLevel // sorted ask levels
    Timestamp time.Time
    Hash      string       // deduplication hash
}

type PriceLevel struct {
    Price float64
    Size  float64
}
```

### Redis HSET Format

```
Key:    book:{exchange}:{market_id}
Fields: bid   <best bid price, string>
        ask   <best ask price, string>
        ts    <unix milliseconds, string>
```

Example:

```
HSET book:polymarket:BTC-100K bid "0.73" ask "0.74" ts "1738964123000"
```

Best-price selection: highest bid, lowest ask. `"0"` if no levels present.

### ArbitrageEvent (internal/adapter/unified_book.go)

```go
type ArbitrageEvent struct {
    Pair        MarketPair
    Direction   ArbitrageDirection   // ArbPolyBidKalshiAsk | ArbKalshiBidPolyAsk
    BidExchange Exchange
    AskExchange Exchange
    Bid         float64
    Ask         float64
    Spread      float64              // bid - ask (positive = opportunity)
    Timestamp   time.Time
}
```

---

## 3. Concurrency Model

### Non-Blocking Fan-Out

Every channel send in the pipeline uses `select { case ch <- msg: default: }`.
A slow consumer is silently dropped; other subscribers are unaffected.
This prevents a single stalled reader from back-pressuring the entire pipeline.

### Goroutine Layout

```
WSClient                    Broadcaster                  Consumers
┌──────────┐  raw []byte   ┌────────────┐  BookUpdate  ┌──────────────┐
│ readLoop ├──────────────►│ per-source │──────────────►│ RedisWriter  │
│ writeLoop│  (fan-out to  │ goroutine  │  (fan-out)   │  (ingest +   │
│ reconnect│   subs)       │  × N       │              │   flush)     │
└──────────┘               └────────────┘              └──────────────┘
                                │                      ┌──────────────┐
                                └─────────────────────►│ UnifiedBook  │
                                                       │ (2 goroutines│
                                                       │  per pair)   │
                                                       └──────────────┘
                                                       ┌──────────────┐
                                                       └►CircuitBreaker│
                                                        │ (1 goroutine)│
                                                        └──────────────┘
```

### Buffer Sizes

| Location | Size |
|---|---|
| WSClient outbox | 256 |
| WSClient subscriber channels | 512 |
| Broadcaster filtered subscription | 256 |
| Broadcaster unified (`SubscribeAll`) | 512 |
| RedisWriter internal buffer | 1024 |
| UnifiedBook events channel | 256 |

### Locking Strategy

- **`atomic.Int32`** — WSClient circuit state (lock-free hot path).
- **`sync.RWMutex`** — read-heavy maps: Broadcaster subscribers, CircuitBreaker market/connection state, UnifiedBook pair state.
- **`sync.Mutex`** — write-heavy state: RedisWriter duplicate map, TunnelManager tunnel map.

---

## 4. Safety Invariants (CircuitBreaker)

`CanTrade(exchange, marketID) bool` is the single gate before any order execution.
It checks four conditions **in order**; all must pass:

| # | Check | Threshold | Rationale |
|---|---|---|---|
| 1 | **Manual Halt** | `halted == false` | Emergency kill switch via `ManualHalt()` / `Resume()`. |
| 2 | **Connection Health** | `WSClient.Circuit() == CircuitClosed` | WS must have active heartbeat. |
| 3 | **Data Freshness** | `now - LastUpdate ≤ 1 000 ms` | Stale book data must not drive orders. |
| 4 | **Cool-Off Period** | `now - RecoveredAt ≥ 2 000 ms` | After recovery, wait before trading to let books stabilise. |

### Recovery Transition

When a market receives a `BookUpdate` after being unhealthy, `RecoveredAt` is set to
`now`. Trading resumes only after the 2 s cool-off elapses. This prevents acting on
the first (potentially incomplete) snapshot after reconnect.

### External Triggers

- `MarkStale(exchange, marketID)` — force a market unhealthy from outside.
- `WatchConnection(exchange, ws)` — register a WSClient for heartbeat monitoring.

### Testability

`nowFunc func() time.Time` is injected at construction, enabling deterministic
tests without sleeps.

---

## 5. Lessons Learned

### Windows / Docker Socket Permissions

The signer runs as non-root UID `10001` inside a `scratch` container and communicates
via a Unix Domain Socket on a shared Docker volume. On certain host OS configurations,
the volume directory is owned by `root`, so the signer cannot bind its socket.

**Fix:** An `init-permissions` init-container (`alpine`, running as `root`) executes
`chown -R 10001:10001 /tmp` on the shared volume before the signer starts.
`depends_on` ensures ordering.

### IPC_LOCK Capability

`memguard` uses `mlock()` to pin secret key material in RAM (preventing swap).
Docker drops this capability by default. Both the signer service and the test runner
require `cap_add: [IPC_LOCK]` in `docker-compose.yml`.

### Subscribe Before Connect

In PolyAdapter (and by extension KalshiAdapter), the adapter subscribes to the
WSClient's fan-out channel in `New()`, not in `Run()`. This eliminates a race where
early messages arrive between `Connect()` and `Subscribe()` and are silently lost.

### Deterministic Integration Tests

All time-dependent assertions (staleness, cool-off) use an injectable `fakeClock`
rather than `time.Sleep`. This makes the full pipeline integration test
(`internal/adapter/integration_test.go`) fast and non-flaky.

---

## 6. Phase 3 Entry Points

The Execution Engine should integrate with:

1. **`UnifiedBook.Events() <-chan ArbitrageEvent`** — triggers for order placement.
2. **`CircuitBreaker.CanTrade(exchange, marketID) bool`** — must gate every order.
3. **`TunnelManager.Open() / Send()`** — for per-user authenticated order submission.
4. **`Signer.Sign()`** (Phase 1) — for EIP-712 order signing on Polymarket.
5. **Redis `book:{exchange}:{market_id}`** — read current best prices for limit pricing.
