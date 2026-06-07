-- Database schema for the financial insights platform (see ARCHITECTURE.md).
-- Applied to the `fininsights` database. rag.go also applies this on startup;
-- this file lets you set the schema up by hand or for a fresh clone.

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
