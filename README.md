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

## Frontend/Query Architecture Plan
- Frontend sends user's request to backend API gateway
- Backend makes gRPC request to RAG service
    - gRPC so we can stream the tokens as LLM generates
- RAG service does embedding of query, searching database, building prompt, asking LLM, streaming back tokens over gRPC
- Backend takes streamed tokens and sends them back to the frontend
- Frontend displays the tokens as it gets them

## Hosting
- Kafka: Hard to find fully free Kafka hosting providers (most are 30 day trials, alternative would be doing it in a free VM on some cloud provider), might self host and record demo video?
- vector db: Pinecone?
- Other services: could be in other VMs in cloud providers or bare minimum all self host and record demo video
