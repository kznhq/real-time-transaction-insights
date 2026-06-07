"""Embedding service.

Plain Kafka consumer on the `transactions` topic (consumer group `embedder`).
For each raw transaction it:
  1. Upserts the raw record into the `transactions` table.
  2. Builds a short text description and embeds it with the local Ollama
     `nomic-embed-text` model.
  3. Upserts the vector into the `transaction_embeddings` table.
Both writes happen in one DB transaction, so the embedding can never be orphaned.
This service owns the `transactions` table; StreamsApp.java owns only the
`aggregations` table (see ARCHITECTURE.md).

Each transaction is printed to the console before embedding, for debugging.
"""

import json
import os

import psycopg2
import requests
from confluent_kafka import Consumer
from dotenv import load_dotenv

load_dotenv()

KAFKA_BROKER = os.getenv("KAFKA_BROKER", "localhost:9092")
DATABASE_URL = os.getenv(
    "DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/fininsights"
)
OLLAMA_HOST = os.getenv("OLLAMA_HOST", "http://localhost:11434")
TOPIC = "transactions"
EMBED_MODEL = "nomic-embed-text"


def build_content(txn):
    """A plain-text representation, e.g. '$52.30 at Chickfila (food) on 2025-06-01'."""
    date = txn["timestamp"][:10]
    return f"${txn['amount']:.2f} at {txn['merchant']} ({txn['category']}) on {date}"


def embed(text):
    """Return a 768-dim embedding for `text` from Ollama."""
    resp = requests.post(
        f"{OLLAMA_HOST}/api/embeddings",
        json={"model": EMBED_MODEL, "prompt": text},
    )
    resp.raise_for_status()
    return resp.json()["embedding"]


def upsert_transaction(cur, txn):
    """Idempotently store the raw transaction. The record is immutable, so on a
    duplicate (re-processed message) we leave the existing row untouched."""
    cur.execute(
        """
        INSERT INTO transactions (id, timestamp, merchant, amount, category)
        VALUES (%s, %s, %s, %s, %s)
        ON CONFLICT (id) DO NOTHING
        """,
        (txn["id"], txn["timestamp"], txn["merchant"], txn["amount"], txn["category"]),
    )


def upsert_embedding(cur, txn, vector, content):
    vector_str = "[" + ",".join(str(v) for v in vector) + "]"
    cur.execute(
        """
        INSERT INTO transaction_embeddings (id, transaction_id, embedding, content)
        VALUES (%s, %s, %s::vector, %s)
        ON CONFLICT (id) DO UPDATE
            SET embedding = EXCLUDED.embedding,
                content = EXCLUDED.content
        """,
        (txn["id"], txn["id"], vector_str, content),
    )


def main():
    conn = psycopg2.connect(DATABASE_URL)
    consumer = Consumer(
        {
            "bootstrap.servers": KAFKA_BROKER,
            "group.id": "embedder",
            "auto.offset.reset": "earliest",
        }
    )
    consumer.subscribe([TOPIC])
    print(f"Embedding transactions from '{TOPIC}' (Ctrl+C to stop)...\n")
    try:
        while True:
            msg = consumer.poll(1.0)
            if msg is None:
                continue
            if msg.error():
                print(f"Consumer error: {msg.error()}")
                continue

            txn = json.loads(msg.value())
            print(f"Embedding: {json.dumps(txn)}")  # debug

            content = build_content(txn)
            vector = embed(content)
            # One transaction: raw record + embedding commit together (or not at all).
            with conn:
                with conn.cursor() as cur:
                    upsert_transaction(cur, txn)
                    upsert_embedding(cur, txn, vector, content)
    except KeyboardInterrupt:
        print("\nStopping...")
    finally:
        consumer.close()
        conn.close()
        print("Stopped.")


if __name__ == "__main__":
    main()
