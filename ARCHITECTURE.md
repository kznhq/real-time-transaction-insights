# Financial Insights Platform — Architecture

A local real-time financial insights platform. Simulated transactions stream through a single Kafka topic with two independent consumers: an embedding service that stores each raw transaction in PostgreSQL and encodes it into pgvector, and a Kafka Streams app that computes per-merchant aggregations and writes them to PostgreSQL. Both data stores are queried by a RAG pipeline exposed through a React frontend. All inference runs locally via Ollama — no external API keys required.

---

## Stack at a glance

| Component | Language / Tech | File |
|---|---|---|
| Transaction producer | Python | `producer.py` |
| Kafka Streams enrichment | Java | `StreamsApp.java` |
| Embedding service | Python | `embedder.py` |
| RAG service | Go | `rag.go` |
| API gateway | Go | `gateway.go` |
| Frontend | React (Vite) | `App.jsx` |
| Database | PostgreSQL + pgvector | schema applied at startup |

All services run locally in separate terminal panes (tmux recommended). No Docker required.

---

## Local infrastructure

### Kafka (KRaft mode, no Zookeeper)
- Run a single-node Kafka broker locally using KRaft mode.
- One topic must exist before starting any service:
  - `transactions` — raw simulated events (produced by `producer.py`)
- Both `StreamsApp.java` and `embedder.py` are independent consumers of this single topic, each in their own consumer group.
- Default broker: `localhost:9092`

### Ollama (local inference)
- Run Ollama locally: `ollama serve` (default: `http://localhost:11434`)
- Pull both models before starting any service:
  ```
  ollama pull nomic-embed-text   # embeddings — 274MB, 768-dim
  ollama pull phi3.5             # completions — ~2.2GB at 4-bit quant
  ```
- Both models fit within 4GB VRAM (GTX 1650 Ti). Do not run them simultaneously in separate processes — Ollama queues requests internally, so this is safe.
- No API key required.

### PostgreSQL + pgvector
- Single local Postgres instance.
- On startup, the RAG service (`rag.go`) applies the schema if tables don't exist.
- Schema:
  ```sql
  CREATE EXTENSION IF NOT EXISTS vector;

  CREATE TABLE IF NOT EXISTS transactions (
    id UUID PRIMARY KEY,
    timestamp TIMESTAMPTZ NOT NULL,
    merchant TEXT NOT NULL,
    amount NUMERIC(12,2) NOT NULL,
    category TEXT
  );

  CREATE TABLE IF NOT EXISTS aggregations (
    merchant TEXT PRIMARY KEY,
    total_revenue NUMERIC(12,2),
    avg_amount NUMERIC(12,2),
    transaction_count INT,
    anomaly_flag BOOLEAN,
    updated_at TIMESTAMPTZ
  );

  CREATE TABLE IF NOT EXISTS transaction_embeddings (
    id UUID PRIMARY KEY,
    transaction_id UUID REFERENCES transactions(id),
    embedding vector(768),
    content TEXT
  );

  CREATE INDEX IF NOT EXISTS embedding_idx
    ON transaction_embeddings USING hnsw (embedding vector_cosine_ops);
  ```
- Default connection: `postgresql://localhost:5432/fininsights`

---

## Data flow

```
producer.py
    │
    │  publishes raw transaction JSON
    ▼
Kafka: transactions topic
    │
    ├───────────────────────────────────────────────┐
    │  consumer group: streams-app                  │  consumer group: embedder
    ▼                                               ▼
StreamsApp.java                               embedder.py
Kafka Streams consumer                        Plain Kafka consumer
Computes per-merchant aggregations:           For each raw transaction (one DB txn):
- total revenue                               1. Upsert raw record into transactions table
- average amount                              2. Build plain-text representation
- transaction count                           3. Embed via Ollama nomic-embed-text
- anomaly flag                                4. Upsert vector into transaction_embeddings
Writes per-merchant stats to the
aggregations table in PostgreSQL.
```

At query time:

```
React App.jsx
    │  HTTP POST /query  (user's natural language question)
    ▼
gateway.go
    │  gRPC streaming call
    ▼
rag.go
    │  1. Embed the query (Ollama nomic-embed-text)
    │  2. Cosine similarity search on transaction_embeddings (pgvector)
    │  3. Fetch related aggregations from PostgreSQL
    │  4. Build prompt with retrieved context
    │  5. Stream response from Ollama (phi3.5)
    ▼
gateway.go
    │  Server-Sent Events (SSE)
    ▼
React App.jsx  ← renders tokens as they arrive
```

---

## Component specs

### `producer.py`
- Produces synthetic transaction JSON to the `transactions` Kafka topic every ~0.5–2s.
- Each message:
  ```json
  {
    "id": "<uuid>",
    "timestamp": "<ISO8601>",
    "merchant": "<string>",
    "amount": <float>,
    "category": "<string>"
  }
  ```
- Library: `confluent-kafka-python`
- Run: `python producer.py`

---

### `StreamsApp.java`
- Kafka Streams application. Single Java file with a `main` method. Consumer group: `streams-app`.
- Consumes `transactions`, computes per-merchant aggregations using a tumbling window (1 minute), marks transactions as anomalous if amount > 2 standard deviations from the merchant's rolling mean.
- Writes directly to PostgreSQL on each record:
  1. Upserts updated per-merchant stats into the `aggregations` table.
- Does not write the raw `transactions` table — that is `embedder.py`'s responsibility.
- Does not publish to any Kafka topic. No output topic.
- Dependencies (Maven or Gradle inline): `kafka-streams`, `jackson-databind`, `postgresql` JDBC driver
- Run: `mvn exec:java` or `java -cp <jar> StreamsApp`

---

### `embedder.py`
- Plain Kafka consumer on the `transactions` topic. Consumer group: `embedder`. Single Python file.
- For each raw transaction message, in a single DB transaction:
  1. Upserts the raw record into the `transactions` table (`ON CONFLICT (id) DO NOTHING` — the record is immutable).
  2. Builds a plain-text representation (e.g. `"$52.30 at Starbucks (food) on 2025-06-01"`).
  3. Calls Ollama `nomic-embed-text` to get a 768-dim vector (`POST http://localhost:11434/api/embeddings`).
  4. Upserts the vector and content into `transaction_embeddings`.
- Owns the `transactions` table. Steps 1 and 4 commit together, so an embedding can never reference a missing transaction. Does not write the `aggregations` table — that is `StreamsApp.java`'s responsibility.
- Libraries: `confluent-kafka-python`, `psycopg2`, `requests`
- Env vars: `DATABASE_URL`, `OLLAMA_HOST` (default: `http://localhost:11434`)
- Run: `python embedder.py`

---

### `rag.go`
- gRPC server. Single `.go` file. Defines and implements the proto service inline (generate proto stubs separately or embed them).
- Proto service:
  ```protobuf
  service RAG {
    rpc Query(QueryRequest) returns (stream QueryResponse);
  }
  message QueryRequest { string question = 1; }
  message QueryResponse { string token = 1; }
  ```
- On each `Query` call:
  1. Embed the question via Ollama `nomic-embed-text` (`POST http://localhost:11434/api/embeddings`) to get a 768-dim vector.
  2. Run pgvector cosine search: `SELECT content FROM transaction_embeddings ORDER BY embedding <=> $1 LIMIT 5`.
  3. Fetch latest aggregations from `aggregations` table.
  4. Build a system prompt with retrieved context.
  5. Stream a completion from Ollama `phi3.5` (`POST http://localhost:11434/api/chat` with `"stream": true`).
  6. Forward each token chunk back to the gRPC stream.
- Libraries: `google.golang.org/grpc`, `github.com/pgvector/pgvector-go`, `github.com/lib/pq`
- Env vars: `DATABASE_URL`, `OLLAMA_HOST` (default: `http://localhost:11434`)
- Listens on: `localhost:50051`
- Run: `go run rag.go`

---

### `gateway.go`
- HTTP server exposing one endpoint: `POST /query`.
- Accepts JSON body `{ "question": "..." }`.
- Makes a gRPC streaming call to `rag.go` at `localhost:50051`.
- Responds with `Content-Type: text/event-stream` (SSE).
- Forwards each token from the gRPC stream as `data: <token>\n\n`.
- Sends `data: [DONE]\n\n` when the stream ends.
- Libraries: `google.golang.org/grpc`, standard `net/http`
- Listens on: `localhost:8080`
- CORS: allow `http://localhost:5173` (Vite default)
- Run: `go run gateway.go`

---

### `App.jsx`
- Single-file React app (Vite project, but all logic in `App.jsx`).
- UI: a text input for the user's question, a submit button, and a response area that streams tokens in as they arrive.
- On submit:
  1. `POST http://localhost:8080/query` with `{ "question": "..." }`.
  2. Read the response as an SSE stream using the `EventSource` API or `fetch` with a `ReadableStream`.
  3. Append each token to the displayed response.
  4. Stop on `[DONE]`.
- Style: minimal inline styles or a single CSS file, no component libraries needed.
- Run: `npm run dev` (Vite, port 5173)

---

## Environment variables

Create a `.env` file (or export in each shell pane):

```
DATABASE_URL=postgresql://localhost:5432/fininsights
KAFKA_BROKER=localhost:9092
OLLAMA_HOST=http://localhost:11434
```

---

## Startup order

Start services in this order to avoid connection errors:

1. Ollama (`ollama serve`) — must be running before embedder or RAG service start
2. Kafka broker (KRaft)
3. PostgreSQL
4. `producer.py` — begin producing transactions
5. `StreamsApp.java` — begin aggregating and writing to Postgres
6. `embedder.py` — begin embedding raw transactions
7. `rag.go` — gRPC RAG server (applies DB schema on startup)
8. `gateway.go` — HTTP gateway
9. React dev server (`npm run dev`)

---

## Key constraints

- Every component is a **single file**. No multi-file services, no internal packages.
- All services run **locally**; no cloud infrastructure, no Docker.
- The Go files (`rag.go`, `gateway.go`) should be runnable with `go run <file>.go` without a module workspace — use a single `go.mod` at the repo root covering both.
- The proto definition for the gRPC interface lives in `rag.proto` at the repo root. Generated stubs (`rag_grpc.pb.go`, `rag.pb.go`) are committed alongside the source.
- Ollama is the only inference dependency. No external API keys. Models used: `nomic-embed-text` (embeddings, 768-dim) and `phi3.5` (completions). Both are called via Ollama's HTTP API at `localhost:11434`.
