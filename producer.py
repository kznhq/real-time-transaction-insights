"""Synthetic transaction producer.

Publishes a stream of fake transaction JSON to the Kafka `transactions` topic,
one every ~0.5-2s, and echoes each to the terminal. Matches the message schema
in ARCHITECTURE.md.
"""

import json
import os
import random
import time
import uuid
from datetime import datetime, timezone

from confluent_kafka import Producer
from dotenv import load_dotenv

load_dotenv()

KAFKA_BROKER = os.getenv("KAFKA_BROKER", "localhost:9092")
TOPIC = "transactions"

# (merchant, category, typical amount range) — keeps amounts plausible per merchant.
MERCHANTS = [
    ("Chickfila", "food", (15, 45)),
    ("Coffee", "food", (10, 30)),
    ("Whole Foods", "groceries", (10, 180)),
    ("Cub Foods", "groceries", (30, 200)),
    ("eBay", "shopping", (5, 400)),
    ("Uniqlo", "shopping", (10, 100)),
    ("Shell", "fuel", (20, 90)),
    ("BP", "fuel", (40, 80)),
    ("PlayStation", "entertainment", (8, 20)),
    ("Delta Airlines", "travel", (120, 900)),
    ("Sun Country Airlines", "travel", (70, 300)),
    ("CVS Pharmacy", "health", (4, 75)),
    ("Walgreens", "health", (10, 50)),
    ("Home Depot", "home", (15, 500)),
    ("Furniture Store", "home", (100, 500)),
    ("Steam", "entertainment", (10, 17)),
]


def make_transaction():
    merchant, category, (low, high) = random.choice(MERCHANTS)
    return {
        "id": str(uuid.uuid4()),
        "timestamp": datetime.now(timezone.utc).isoformat(),
        "merchant": merchant,
        "amount": round(random.uniform(low, high), 2),
        "category": category,
    }


def delivery_report(err, msg):
    if err is not None:
        print(f"Delivery failed: {err}")


def main():
    producer = Producer({"bootstrap.servers": KAFKA_BROKER})
    print(f"Producing transactions to '{TOPIC}' at {KAFKA_BROKER} (Ctrl+C to stop)...\n")
    try:
        while True:
            txn = make_transaction()
            producer.produce(
                TOPIC,
                key=txn["merchant"],
                value=json.dumps(txn),
                callback=delivery_report,
            )
            producer.poll(0)  # serve delivery callbacks
            print(json.dumps(txn))
            time.sleep(random.uniform(0.5, 2.0))
    except KeyboardInterrupt:
        print("\nStopping, flushing pending messages...")
    finally:
        producer.flush()
        print("Stopped.")


if __name__ == "__main__":
    main()
