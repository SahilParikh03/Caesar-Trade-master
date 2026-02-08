System Design & "Source of Truth"
1. Core Architecture: The "Speed-First" Stack

    Backend: Golang (Primary). Chosen for low-latency concurrency, efficient networking via netpoller, and high-performance WebSocket handling.

    Infrastructure: AWS (Region: us-east-1 for Kalshi proximity).

    Communication:

        External: WebSockets (WSS) for real-time price feeds.

        Internal: gRPC over Unix Domain Sockets (UDS) for inter-process communication (IPC) between the Backend and Signer.

    State Management: Redis (In-memory cache for live order books) + PostgreSQL (Market mappings, user profiles, trade history).

2. Hardware & Networking Optimization

To achieve professional-grade execution, we bypass standard OS overhead:

    Interrupt Affinity: NIC interrupts are bound to Core 0; Trading Engine runs on isolated Cores 1-3 to prevent cache flushes.

    Network Tuning: TCP_NODELAY enabled; Go Read/WriteBuffer sized to MTU (1500-16384 bytes) to minimize syscalls.

    Scaling: "Fan-Out" architecture.

        Broadcaster: One connection per market to exchange → broadcast to N users.

        Private Tunnels: One authenticated WSS per user (proxied via IP pool to avoid rate-limiting).

3. Execution & Custody Logic
A. The "Session Key" (Polymarket)

We utilize a non-custodial Account Abstraction (ERC-4337) model.

    User connects wallet.

    User authorizes a Session Key (stored in memory via mlock) for a specific time/value limit.

    Terminal signs orders instantly using the Session Key—no UI popups required.

B. The Signer Microservice

    Security: Isolated process with Zero Disk Access. Keys fetched via AWS IAM Roles (Instance Identity) from KMS—no hardcoded secrets.

    Latency: Communicates with the main Go backend via UDS to avoid the TCP stack.

4. Market Mapping & Logic Engine
A. Adversarial Consensus (LLM Judge)

To pair Polymarket and Kalshi markets without false positives:

    Model A (GPT-4o) extracts resolution criteria.

    Model B (Claude 3.5) extracts resolution criteria.

    Judge (Llama 3) compares both: "Are these logically identical? (Ignore formatting)."

    Status: Verified (if Yes), Unverified (if No/Unsure).

B. Dispute & Safety Protocols

    UMA Monitoring: Backend monitors UMA Oracle contracts directly for PriceDisputed events (bypassing API lag).

    Circuit Breaker: If data is stale (>500ms heartbeat loss) or a market is disputed, the Execute button is hard-disabled. No Data > Stale Data.

    Slippage Guard: During disputes, max slippage is capped at 0.1% from the last undisputed price.

5. Transaction Resilience (Polygon)

To handle deep re-orgs and mempool congestion:

    Confidence Gauge:

        0-1s: Broadcasting (ACK from RPC).

        1-5s: Mined (Included in block, reversible).

        30s+: Confirmed (~16-32 blocks).

    Nonce Management: If a trade stalls, re-send a Replacement Transaction using the same Nonce with a 20%+ Gas increase (Exponential Backoff).

6. Data Schemas (Simplified)
PostgreSQL: Market Map
SQL

CREATE TABLE market_pairs (
    internal_id UUID PRIMARY KEY,
    poly_condition_id TEXT UNIQUE,
    kalshi_ticker TEXT UNIQUE,
    logic_status VARCHAR(20), -- 'Verified', 'Unverified'
    is_halted BOOLEAN DEFAULT FALSE,
    last_undisputed_price DECIMAL
);

Redis: Live Book (HSET)
Bash

KEY: "book:poly:btc-100k"
FIELDS: { "bid": "0.55", "ask": "0.57", "ts": "1707350000" }