# real-time-transaction-insights
Project to learn developing end-to-end ML systems combining data streaming/processing pipelines, embeddings-based retrieval, and distributed services with Go, Python, Kafka, Flink, PostgreSQL and pgvector, and gRPC.
Stretch goal is adding LLM for natural language queries and RAG experience.

I know I need a better name.

Choosing to start this during the school year was a move but I want to try and work on this in my free time. 

## Pipeline Plan
- Simulated transactions are published to Kafka
- Flink does some processing/calculations on that data (ex. revenue, average transaction amount, etc)
- Embedding service is Kafka consumer, takes transactions, calculates vector embeddings, stores that in Pinecone (?) vector db
- User can interact with LLM through some React (?) frontend with natural language queries answered through RAG
