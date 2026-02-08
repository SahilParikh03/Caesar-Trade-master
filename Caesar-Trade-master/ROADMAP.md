# Caesar — Implementation Roadmap

## Context
Caesar is a greenfield institutional-grade prediction market trading terminal. It unifies Polymarket (on-chain) and Kalshi (regulated/fiat) into a single "Command Center" for arbitrage, scanning, and hedging. This plan is ordered by technical dependency — nothing is built before its prerequisites exist.

---

## Phase 0: Repository & CI Foundation
| # | Ticket | Depends On | Description |
|---|--------|------------|-------------|
| 0.1 | Init Go module & folder scaffold | — | `go mod init github.com/caesar-terminal/caesar`, create `/cmd`, `/internal`, `/pkg`, `/proto`, `/deploy`, `/frontend`, `/scripts` |
| 0.2 | Proto toolchain | 0.1 | `buf.yaml`, protoc-gen-go/grpc stubs, Makefile target |
| 0.3 | Docker Compose (dev) | 0.1 | Postgres 16, Redis 7, LocalStack (KMS mock) |
| 0.4 | CI pipeline (GitHub Actions) | 0.1 | Lint (`golangci-lint`), test, build gates |
| 0.5 | `.env.example` + config loader | 0.1 | Viper-based config; all secrets via env/IAM — zero disk |

## Phase 1: Signer Microservice ("The Heart")
| # | Ticket | Depends On | Description |
|---|--------|------------|-------------|
| 1.1 | Define `signer.proto` | 0.2 | RPCs: `SignOrder`, `SignCancel`, `GetSessionStatus`. Messages: EIP-712 typed data |
| 1.2 | UDS gRPC server skeleton | 0.2, 0.5 | Listener on `/var/run/caesar/signer.sock`, graceful shutdown |
| 1.3 | AWS KMS key fetch (IAM Roles) | 0.5 | `sts:AssumeRole` → `kms:Decrypt` flow; LocalStack mock for dev |
| 1.4 | Session Key manager | 1.3 | In-memory store with `mlock`, TTL expiry, value-limit enforcement |
| 1.5 | EIP-712 signing logic | 1.1, 1.4 | Typed structured data hashing + ECDSA sign using session key |
| 1.6 | Signer integration tests | 1.2, 1.5 | Round-trip: client → UDS → sign → verify on-chain compatible |
| 1.7 | Signer Dockerfile | 1.6 | Minimal scratch image, no shell, `CAP_IPC_LOCK` for mlock |

## Phase 2: Exchange Adapters & WebSocket Layer
| # | Ticket | Depends On | Description |
|---|--------|------------|-------------|
| 2.1 | WebSocket connection manager | 0.5 | Reconnect logic, heartbeat (500ms timeout), `TCP_NODELAY`, buffer sizing |
| 2.2 | Polymarket adapter | 2.1 | Subscribe to order book streams, parse into internal `BookUpdate` struct |
| 2.3 | Kalshi adapter | 2.1 | REST auth + WSS feed, normalize to same `BookUpdate` struct |
| 2.4 | Broadcaster (fan-out) | 2.2, 2.3 | One conn per market → broadcast to N subscriber channels |
| 2.5 | Private Tunnel manager | 2.1 | Per-user authenticated WSS, IP pool rotation, rate-limit evasion |
| 2.6 | Redis book writer | 2.4, 0.3 | Write `HSET book:{exchange}:{market}` on every tick |
| 2.7 | Unified Order Book builder | 2.6 | Merge Poly + Kalshi books for same market pair into a single depth view; flag price discrepancies for arbitrage |
| 2.8 | Circuit Breaker | 2.4 | Heartbeat monitor; stale >500ms → disable execution; dispute halt |
| 2.9 | Adapter integration tests | 2.7, 2.8 | Mock exchange feeds, verify Redis state, unified book merge, circuit breaker triggers |

## Phase 3: Order Execution Engine
| # | Ticket | Depends On | Description |
|---|--------|------------|-------------|
| 3.1 | Order types & validation | 1.5, 2.6 | Limit, Market, Stop-Loss; slippage guard (0.1% during disputes) |
| 3.2 | Execution pipeline | 3.1 | Read Redis book → build order → call Signer (UDS) → submit to exchange |
| 3.3 | Cross-platform hedged execution | 3.2, 2.7 | Single-click: buy on Exchange A + sell on Exchange B simultaneously; atomic-intent with rollback if one leg fails |
| 3.4 | Nonce manager (Polygon) | 3.2 | Track pending nonces; replacement tx (+20% gas, exponential backoff) |
| 3.5 | Confidence gauge | 3.4 | State machine: Broadcasting → Mined → Confirmed (16-32 blocks) |
| 3.6 | UMA dispute monitor | 2.8 | Watch `PriceDisputed` events on-chain; trigger circuit breaker + slippage cap |
| 3.7 | Execution engine tests | 3.5, 3.6 | End-to-end with mock signer + mock exchange; verify hedged execution rollback |

## Phase 4: Market Mapping & LLM Judge
| # | Ticket | Depends On | Description |
|---|--------|------------|-------------|
| 4.1 | PostgreSQL schema + migrations | 0.3 | `market_pairs` table, indexes on `poly_condition_id`, `kalshi_ticker` |
| 4.2 | Market ingestion workers | 2.2, 2.3, 4.1 | Periodic fetch of available markets from both exchanges |
| 4.3 | LLM Judge — Model A (GPT-4o) | 4.2 | Extract resolution criteria from Polymarket market description |
| 4.4 | LLM Judge — Model B (Claude) | 4.2 | Extract resolution criteria from Kalshi market description |
| 4.5 | LLM Judge — Arbiter (Llama 3) | 4.3, 4.4 | Compare criteria: "Logically identical? (Ignore formatting)" → Verified/Unverified |
| 4.6 | Mapping API | 4.5 | CRUD endpoints for `market_pairs`; manual override for disputes |
| 4.7 | Judge pipeline tests | 4.5 | Golden-set of known pairs + known non-pairs; precision/recall metrics |

## Phase 5: Frontend — "The Cockpit"
| # | Ticket | Depends On | Description |
|---|--------|------------|-------------|
| 5.1 | Next.js project scaffold | 0.1 | App router, Tailwind, TypeScript strict mode |
| 5.2 | WebSocket client hook | 5.1, 2.4 | `useBookFeed(market)` — connects to Broadcaster, parses `BookUpdate` |
| 5.3 | Unified Order Book / depth chart | 5.2, 2.7 | Side-by-side bid/ask ladders for both exchanges; highlight arbitrage spread in real-time |
| 5.4 | Market scanner dashboard | 5.2, 4.6 | High-density view: news feed aggregation, volume spikes, price action across both venues; "first-mover" signal detection |
| 5.5 | Wallet connect + session key UI | 5.1, 1.4 | WalletConnect v2 / injected; session key authorization flow |
| 5.6 | Unified Balance view | 5.5, 2.3 | Aggregate Polygon wallet + Kalshi fiat balance into single "Total Exposure" panel; cross-chain + fiat invisible to trader |
| 5.7 | Trade execution panel | 5.3, 5.5, 3.2 | Order form (Limit, Market, Stop-Loss) → API → execution pipeline; shows confidence gauge; single-click hedge button |
| 5.8 | Circuit breaker UI | 5.7, 2.8 | Hard-disable Execute button; show stale/disputed status; prevent trading into broken markets |
| 5.9 | Market pair browser | 5.1, 4.6 | Browse Verified/Unverified pairs; search/filter |
| 5.10 | Frontend E2E tests | 5.8, 5.9 | Playwright against dev Docker Compose stack |

## Phase 6: Deployment & Hardening
| # | Ticket | Depends On | Description |
|---|--------|------------|-------------|
| 6.1 | Hetzner bare-metal provisioning | All Phase 1-5 | Terraform/Ansible; CPU isolation (cores 1-3), interrupt affinity (core 0) |
| 6.2 | AWS IAM + KMS production setup | 6.1 | Production KMS keys, IAM roles, instance profiles |
| 6.3 | VPC + networking | 6.1 | Private subnet for Signer, NAT for outbound, security groups |
| 6.4 | Monitoring & alerting | 6.1 | Prometheus + Grafana; alerts on heartbeat loss, nonce stalls, circuit breaker trips |
| 6.5 | Load testing | 6.4 | Simulate N users, measure p99 latency through full pipeline |
| 6.6 | Security audit checklist | 6.5 | Zero-disk verification, log audit (no leaked keys), syscall whitelist review |

---

## Success Criteria
- Arbitrage opportunity identified within **500ms** of occurrence
- Hedged position across both platforms executable with **single click**
- Total directional risk visible from **one screen** (Unified Balance)
- Terminal **auto-prevents** trading into disputed/broken markets (Circuit Breaker)

## Verification
- **Phase 0:** `go build ./...` succeeds, `docker compose up` starts all services, CI passes
- **Phase 1:** Signer integration tests pass; `mlock` verified; no TCP ports exposed
- **Phase 2:** Mock feed → Redis book populated <1ms; unified book merges correctly; circuit breaker fires on heartbeat loss
- **Phase 3:** Full order round-trip <10ms (mock); hedged execution rolls back on partial failure; nonce replacement verified
- **Phase 4:** Judge correctly matches golden-set with >95% precision
- **Phase 5:** E2E tests pass; Execute button disabled when circuit breaker active; arbitrage spread highlighted within 500ms
- **Phase 6:** p99 latency meets target; security audit passes
