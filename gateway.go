// HTTP gateway: exposes POST /query, forwards the question to the RAG gRPC
// service (rag.go) as a streaming call, and relays the streamed tokens back to
// the browser as Server-Sent Events. See ARCHITECTURE.md.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"fininsights/pb"
)

const (
	ragAddr      = "localhost:50051"
	listenAddr   = "localhost:8080"
	allowedOrigin = "http://localhost:5173" // Vite dev server
)

type queryRequest struct {
	Question string `json:"question"`
}

func main() {
	http.HandleFunc("/query", handleQuery)
	log.Printf("Gateway listening on http://%s (POST /query)", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, nil))
}

func handleQuery(w http.ResponseWriter, r *http.Request) {
	// CORS for the Vite frontend.
	w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	log.Printf("Request body: %s", body) // debug

	var req queryRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Connect to the RAG gRPC service.
	conn, err := grpc.NewClient(ragAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		http.Error(w, "failed to connect to RAG service", http.StatusBadGateway)
		log.Printf("grpc dial error: %v", err)
		return
	}
	defer conn.Close()

	client := pb.NewRAGClient(conn)
	stream, err := client.Query(r.Context(), &pb.QueryRequest{Question: req.Question})
	if err != nil {
		http.Error(w, "RAG query failed", http.StatusBadGateway)
		log.Printf("rag query error: %v", err)
		return
	}

	// Stream tokens back as Server-Sent Events.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("stream recv error: %v", err)
			break
		}
		fmt.Fprintf(w, "data: %s\n\n", resp.GetToken())
		flusher.Flush()
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
