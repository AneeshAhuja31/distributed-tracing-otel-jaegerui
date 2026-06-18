# Distributed Tracing Demo

OpenTelemetry + Jaeger + Go Microservices

---

## System Design

```
┌─────────────────────────────────────────────────────────────────┐
│                            CLIENT                               │
│              curl / Postman / Browser                           │
└───────────────────────────┬─────────────────────────────────────┘
                            │  GET /checkout?user_id=&order_id=
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                       API GATEWAY :8080                         │
│                    span: "checkout"                             │
└──────────────┬──────────────────────────┬───────────────────────┘
               │ ① GET /validate          │ ② GET /create (if auth ok)
               ▼                          ▼
┌──────────────────────┐    ┌─────────────────────────────────────┐
│  AUTH SERVICE :8081  │    │        ORDER SERVICE :8082          │
│  span: "validate"    │    │        span: "create"               │
│                      │    └──────────────┬──────────────────────┘
│  - 50ms latency      │                   │
│  - rejects           │        ① GET /process    ② GET /reserve
│    "invalid-user"    │                   │
└──────────────────────┘          ┌────────┴────────┐
                                  ▼                  ▼
                     ┌─────────────────┐  ┌──────────────────────┐
                     │  LLM SVC :8083  │  │ INVENTORY SVC :8084  │
                     │  span:"process" │  │  span: "reserve"     │
                     │  span:"llm-call"│  │  span: "db-lookup"   │
                     │                 │  │  span: "db-reserve"  │
                     │  → Anthropic or │  │                      │
                     │    mock LLM     │  │  stock: item-001: 50 │
                     │  200–800ms delay│  │         item-002: 3  │
                     │  APPROVED /     │  │         item-003: 0  │
                     │  REJECTED       │  └──────────────────────┘
                     └────────┬────────┘
                              │ (if no ANTHROPIC_API_KEY)
                              ▼
                     ┌─────────────────┐
                     │  Mock Decision  │
                     │  Rejects order  │
                     │  ending in "9"  │
                     └─────────────────┘

─────────────────────── OBSERVABILITY ───────────────────────────

  All 5 services  ──► OTLP HTTP :4318 ──► Jaeger UI :16686
  (otlptracehttp)      (in Jaeger)          localhost:16686
```

---

## Services

| Service           | Port | Endpoint    | Span(s)                                      |
|-------------------|------|-------------|----------------------------------------------|
| API Gateway       | 8080 | `/checkout` | `checkout`, `call-auth-service`, `call-order-service` |
| Auth Service      | 8081 | `/validate` | `validate`                                   |
| Order Service     | 8082 | `/create`   | `create`, `call-llm-service`, `call-inventory-service` |
| LLM Service       | 8083 | `/process`  | `process`, `llm-call`                        |
| Inventory Service | 8084 | `/reserve`  | `reserve`, `db-lookup-inventory`, `db-reserve-item` |

---

## Quick Start

**1. Start Jaeger**
```bash
docker run -d --name jaeger -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one:latest
```

**2. Start all services** (separate terminals)
```bash
cd api-gateway       && go run main.go   # :8080
cd auth-service      && go run main.go   # :8081
cd order-service     && go run main.go   # :8082
cd llm-service       && go run main.go   # :8083
cd inventory-service && go run main.go   # :8084
```

> For real LLM calls, set `ANTHROPIC_API_KEY` before running llm-service. Otherwise it uses mock.

**3. Trigger a trace**
```bash
curl "http://localhost:8080/checkout?user_id=user-123&order_id=order-456"
```

**4. View in Jaeger UI**
```
http://localhost:16686
```
Select service `api-gateway` → Find Traces → click a trace to see the full span waterfall.

---

## Test Scenarios

| curl command | Expected result |
|---|---|
| `?user_id=user-123&order_id=order-456` | ✅ 200 Success |
| `?user_id=invalid-user` | ❌ 401 Unauthorized |
| `?order_id=order-9` | ❌ 402 Rejected by LLM (mock) |
| `/reserve?item_id=item-003` direct | ❌ 409 Out of stock |
