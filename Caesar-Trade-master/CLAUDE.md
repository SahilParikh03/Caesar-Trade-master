# CLAUDE.md — Caesar System Memory

## Project Overview
Caesar is an institutional-grade prediction market trading terminal that unifies Polymarket (on-chain) and Kalshi (regulated/fiat) into a single "Command Center" for arbitrage, scanning, and hedging. Think "Bloomberg Terminal for prediction markets."

### Product Vision
- Arbitrage opportunity identified within **500ms** of occurrence
- Hedged position across both platforms executable with **single click**
- Total directional risk visible from **one screen** (Unified Balance)
- Terminal **auto-prevents** trading into disputed/broken markets (Circuit Breaker)

## Tech Stack
- **Backend:** Go (primary) — chosen for low-latency concurrency and netpoller-based networking
- **Frontend:** Next.js ("The Cockpit") — real-time charting and trade execution UI
- **Infrastructure:** AWS `us-east-1` (Kalshi proximity), Hetzner for cost-sensitive workloads
- **Databases:** PostgreSQL (market mappings, user profiles, trade history), Redis (live order books, in-memory cache)
- **IPC:** gRPC over Unix Domain Sockets (UDS) between Backend ↔ Signer
- **External Comms:** WebSockets (WSS) for real-time price feeds
- **Blockchain:** Polygon (ERC-4337 Account Abstraction, EIP-712 signing)

## Naming Conventions
- Go packages: lowercase, single-word (e.g., `signer`, `engine`, `adapter`)
- Go files: snake_case (e.g., `order_book.go`)
- Proto files: snake_case (e.g., `signer_service.proto`)
- Environment variables: SCREAMING_SNAKE_CASE prefixed with `CAESAR_`
- Database tables: snake_case (e.g., `market_pairs`)
- Redis keys: colon-delimited (e.g., `book:poly:btc-100k`)

## Security Rules — "Zero-Disk Access"
These are **non-negotiable** constraints for the Signer microservice:

1. **No hardcoded secrets.** Keys are fetched at runtime via AWS IAM Roles (Instance Identity) from KMS.
2. **No disk persistence of keys.** Session keys are held in memory only, locked via `mlock` to prevent swap.
3. **Process isolation.** The Signer runs as a separate process with minimal syscall surface.
4. **UDS-only communication.** The Signer NEVER exposes a TCP/IP port. All communication is via Unix Domain Sockets.
5. **No logging of sensitive data.** Private keys, session keys, and signatures must NEVER appear in logs.

## Architecture Invariants
- **Circuit Breaker:** If heartbeat loss exceeds 500ms or a market is disputed, the Execute button is hard-disabled.
- **Slippage Guard:** During disputes, max slippage capped at 0.1% from last undisputed price.
- **Fan-Out:** One exchange connection per market → broadcast to N users. Private tunnels are per-user with IP pool rotation.
- **TCP_NODELAY:** Always enabled on trading connections.
- **Nonce Management:** Stalled trades trigger replacement transactions (same nonce, +20% gas, exponential backoff).

## Key Data Schemas
- `market_pairs` table: `internal_id (UUID PK)`, `poly_condition_id`, `kalshi_ticker`, `logic_status`, `is_halted`, `last_undisputed_price`
- Redis live book: `HSET book:{exchange}:{market} bid ask ts`
