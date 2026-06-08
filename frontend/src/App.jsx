// Single-file React app: asks a natural-language question, streams the answer
// back from the gateway (Server-Sent Events) and renders tokens as they arrive.
// See ARCHITECTURE.md. Runs in a Vite project (npm run dev, port 5173).
import { useState } from "react";

const GATEWAY_URL = "http://localhost:8080/query";

export default function App() {
  const [question, setQuestion] = useState("");
  const [answer, setAnswer] = useState("");
  const [loading, setLoading] = useState(false);

  async function handleSubmit(e) {
    e.preventDefault();
    if (!question.trim() || loading) return;

    setAnswer("");
    setLoading(true);
    try {
      const resp = await fetch(GATEWAY_URL, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ question }),
      });
      if (!resp.ok) throw new Error(`Gateway returned ${resp.status}`);

      const reader = resp.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        // SSE events are separated by a blank line.
        const events = buffer.split("\n\n");
        buffer = events.pop(); // keep any incomplete trailing event

        for (const event of events) {
          // An event may have multiple "data:" lines; join them with newlines.
          const data = event
            .split("\n")
            .filter((line) => line.startsWith("data:"))
            .map((line) => (line.startsWith("data: ") ? line.slice(6) : line.slice(5)))
            .join("\n");
          if (data === "") continue;
          if (data === "[DONE]") return;
          setAnswer((prev) => prev + data);
        }
      }
    } catch (err) {
      setAnswer(`Error: ${err.message}`);
    } finally {
      setLoading(false);
    }
  }

  return (
    <div style={styles.page}>
      <h1 style={styles.title}>Financial Insights</h1>
      <form onSubmit={handleSubmit} style={styles.form}>
        <input
          style={styles.input}
          value={question}
          onChange={(e) => setQuestion(e.target.value)}
          placeholder="Ask about your transactions..."
        />
        <button style={styles.button} type="submit" disabled={loading}>
          {loading ? "Thinking..." : "Ask"}
        </button>
      </form>
      <div style={styles.answer}>{answer}</div>
    </div>
  );
}

const styles = {
  page: {
    maxWidth: 640,
    margin: "60px auto",
    padding: "0 16px",
    fontFamily: "system-ui, sans-serif",
  },
  title: { fontSize: 24, marginBottom: 16 },
  form: { display: "flex", gap: 8 },
  input: {
    flex: 1,
    padding: "10px 12px",
    fontSize: 16,
    border: "1px solid #ccc",
    borderRadius: 6,
  },
  button: {
    padding: "10px 18px",
    fontSize: 16,
    border: "none",
    borderRadius: 6,
    background: "#2563eb",
    color: "#fff",
    cursor: "pointer",
  },
  answer: {
    marginTop: 24,
    whiteSpace: "pre-wrap",
    lineHeight: 1.6,
    minHeight: 80,
  },
};
