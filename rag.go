// RAG gRPC service (see ARCHITECTURE.md).
//
// Implements the RAG.Query streaming RPC: embeds the user's question, runs a
// pgvector cosine search over transaction_embeddings, pulls the latest
// per-merchant aggregations, builds a prompt, and streams a completion from the
// local Ollama phi3.5 model back to the caller token by token.
//
// Single-file component. Run with: go run rag.go
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"

	_ "github.com/lib/pq"
	"github.com/pgvector/pgvector-go"
	"google.golang.org/grpc"

	"fininsights/pb"
)

const (
	listenAddr  = "localhost:50051"
	embedModel  = "nomic-embed-text"
	chatModel   = "phi3.5"
	topK        = 5
	maxAggRows  = 10
)

// Applied on startup if the tables don't already exist.
const schemaSQL = `
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
`

func ollamaHost() string {
	if v := os.Getenv("OLLAMA_HOST"); v != "" {
		return v
	}
	return "http://localhost:11434"
}

func databaseURL() string {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		url = "postgresql://postgres:postgres@localhost:5432/fininsights"
	}
	// Local Postgres isn't configured for SSL; lib/pq requires it by default.
	if !strings.Contains(url, "sslmode=") {
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		url += sep + "sslmode=disable"
	}
	return url
}

type server struct {
	pb.UnimplementedRAGServer
	db *sql.DB
}

func main() {
	db, err := sql.Open("postgres", databaseURL())
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		log.Fatalf("db ping: %v", err)
	}
	if err := applySchema(db); err != nil {
		log.Fatalf("apply schema: %v", err)
	}

	lis, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	pb.RegisterRAGServer(s, &server{db: db})
	log.Printf("RAG service listening on %s", listenAddr)
	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func applySchema(db *sql.DB) error {
	for _, stmt := range strings.Split(schemaSQL, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("statement %q: %w", strings.TrimSpace(stmt), err)
		}
	}
	return nil
}

// Query handles one streaming RAG request.
func (s *server) Query(req *pb.QueryRequest, stream pb.RAG_QueryServer) error {
	ctx := stream.Context()
	log.Printf("Query: %q", req.GetQuestion())

	vector, err := embed(ctx, req.GetQuestion())
	if err != nil {
		return fmt.Errorf("embed question: %w", err)
	}

	contexts, err := s.searchSimilar(ctx, vector)
	if err != nil {
		return fmt.Errorf("vector search: %w", err)
	}
	aggregations, err := s.fetchAggregations(ctx)
	if err != nil {
		return fmt.Errorf("fetch aggregations: %w", err)
	}

	systemPrompt := buildPrompt(contexts, aggregations)
	return s.streamCompletion(ctx, systemPrompt, req.GetQuestion(), stream)
}

// searchSimilar returns the content of the topK closest transaction embeddings.
func (s *server) searchSimilar(ctx context.Context, vector pgvector.Vector) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT content FROM transaction_embeddings ORDER BY embedding <=> $1 LIMIT $2",
		vector, topK)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contents []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		contents = append(contents, c)
	}
	return contents, rows.Err()
}

// fetchAggregations returns the top merchants by revenue, formatted as lines.
func (s *server) fetchAggregations(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT merchant, total_revenue, avg_amount, transaction_count, anomaly_flag
		 FROM aggregations ORDER BY total_revenue DESC LIMIT $1`, maxAggRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var merchant string
		var revenue, avg float64
		var count int
		var anomaly bool
		if err := rows.Scan(&merchant, &revenue, &avg, &count, &anomaly); err != nil {
			return nil, err
		}
		line := fmt.Sprintf("- %s: %d transactions, $%.2f total revenue, $%.2f average",
			merchant, count, revenue, avg)
		if anomaly {
			line += " (recent anomaly)"
		}
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

func buildPrompt(contexts, aggregations []string) string {
	var sb strings.Builder
	sb.WriteString("You are a financial insights assistant. Answer the user's question using only the context below. ")
	sb.WriteString("If the context does not contain the answer, say so.\n\n")

	sb.WriteString("Relevant transactions:\n")
	if len(contexts) == 0 {
		sb.WriteString("(none found)\n")
	} else {
		for _, c := range contexts {
			sb.WriteString("- " + c + "\n")
		}
	}

	sb.WriteString("\nPer-merchant aggregates:\n")
	if len(aggregations) == 0 {
		sb.WriteString("(none found)\n")
	} else {
		sb.WriteString(strings.Join(aggregations, "\n"))
		sb.WriteString("\n")
	}
	return sb.String()
}

// --- Ollama HTTP calls ---

func embed(ctx context.Context, text string) (pgvector.Vector, error) {
	body, _ := json.Marshal(map[string]string{"model": embedModel, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaHost()+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return pgvector.Vector{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pgvector.Vector{}, err
	}
	defer resp.Body.Close()

	var out struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return pgvector.Vector{}, err
	}
	return pgvector.NewVector(out.Embedding), nil
}

func (s *server) streamCompletion(ctx context.Context, systemPrompt, question string, stream pb.RAG_QueryServer) error {
	reqBody, _ := json.Marshal(map[string]any{
		"model": chatModel,
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": question},
		},
		"stream": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaHost()+"/api/chat", bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Ollama streams newline-delimited JSON objects.
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var chunk struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Done bool `json:"done"`
		}
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		if chunk.Message.Content != "" {
			if err := stream.Send(&pb.QueryResponse{Token: chunk.Message.Content}); err != nil {
				return err
			}
		}
		if chunk.Done {
			break
		}
	}
	return scanner.Err()
}
