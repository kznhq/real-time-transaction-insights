import java.math.BigDecimal;
import java.math.RoundingMode;
import java.net.URI;
import java.sql.Connection;
import java.sql.DriverManager;
import java.sql.PreparedStatement;
import java.util.Properties;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;

import org.apache.kafka.clients.consumer.ConsumerConfig;
import org.apache.kafka.common.serialization.Serdes;
import org.apache.kafka.streams.KafkaStreams;
import org.apache.kafka.streams.KeyValue;
import org.apache.kafka.streams.StreamsBuilder;
import org.apache.kafka.streams.StreamsConfig;
import org.apache.kafka.streams.kstream.Consumed;
import org.apache.kafka.streams.kstream.Grouped;
import org.apache.kafka.streams.kstream.KStream;
import org.apache.kafka.streams.kstream.Materialized;

/**
 * Kafka Streams aggregation service (see ARCHITECTURE.md).
 *
 * Consumes the `transactions` topic and maintains a running per-merchant
 * aggregate (count, revenue, average) plus an anomaly flag for the latest
 * transaction (amount more than 2 standard deviations above the merchant's
 * mean). On every update it upserts the merchant's row in the `aggregations`
 * table. It does NOT write the raw `transactions` table — embedder.py owns that.
 *
 * Note: the `aggregations` table is keyed by merchant (one row each), so this
 * uses a running aggregate rather than literal tumbling windows, which would
 * produce one row per (merchant, time-window) and not fit the schema.
 */
public class StreamsApp {

    private static final String TOPIC = "transactions";
    private static final ObjectMapper MAPPER = new ObjectMapper();

    // Shared JDBC connection. Default num.stream.threads is 1, so writes are
    // single-threaded; we synchronize defensively anyway.
    private static Connection conn;

    public static void main(String[] args) throws Exception {
        String broker = getenv("KAFKA_BROKER", "localhost:9092");
        String dbUrl = getenv("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/fininsights");

        conn = connect(dbUrl);

        Properties props = new Properties();
        props.put(StreamsConfig.APPLICATION_ID_CONFIG, "streams-app"); // = consumer group
        props.put(StreamsConfig.BOOTSTRAP_SERVERS_CONFIG, broker);
        props.put(StreamsConfig.DEFAULT_KEY_SERDE_CLASS_CONFIG, Serdes.String().getClass());
        props.put(StreamsConfig.DEFAULT_VALUE_SERDE_CLASS_CONFIG, Serdes.String().getClass());
        props.put(ConsumerConfig.AUTO_OFFSET_RESET_CONFIG, "earliest");

        StreamsBuilder builder = new StreamsBuilder();
        KStream<String, String> source = builder.stream(TOPIC, Consumed.with(Serdes.String(), Serdes.String()));

        source
            // Re-key by merchant (producer already keys this way, but be explicit).
            .map((key, value) -> new KeyValue<>(merchantOf(value), value))
            .groupByKey(Grouped.with(Serdes.String(), Serdes.String()))
            // Aggregate value is a small JSON blob: {count, sum, sumsq, last}.
            .aggregate(
                () -> "",
                (merchant, txnJson, aggJson) -> updateStats(aggJson, txnJson),
                Materialized.with(Serdes.String(), Serdes.String()))
            .toStream()
            .foreach(StreamsApp::writeAggregation);

        KafkaStreams streams = new KafkaStreams(builder.build(), props);
        Runtime.getRuntime().addShutdownHook(new Thread(() -> {
            streams.close();
            closeQuietly(conn);
        }));

        System.out.println("StreamsApp running: aggregating '" + TOPIC + "' -> aggregations table (Ctrl+C to stop)...");
        streams.start();
    }

    /** Merge one transaction into the running stats JSON. */
    private static String updateStats(String aggJson, String txnJson) {
        try {
            double amount = MAPPER.readTree(txnJson).get("amount").asDouble();

            long count = 0;
            double sum = 0, sumsq = 0;
            if (aggJson != null && !aggJson.isEmpty()) {
                JsonNode a = MAPPER.readTree(aggJson);
                count = a.get("count").asLong();
                sum = a.get("sum").asDouble();
                sumsq = a.get("sumsq").asDouble();
            }
            count += 1;
            sum += amount;
            sumsq += amount * amount;

            ObjectNode out = MAPPER.createObjectNode();
            out.put("count", count);
            out.put("sum", sum);
            out.put("sumsq", sumsq);
            out.put("last", amount);
            return MAPPER.writeValueAsString(out);
        } catch (Exception e) {
            System.err.println("Failed to parse record, skipping: " + e.getMessage());
            return aggJson == null ? "" : aggJson;
        }
    }

    /** Compute derived metrics and upsert the merchant's aggregations row. */
    private static void writeAggregation(String merchant, String aggJson) {
        try {
            JsonNode a = MAPPER.readTree(aggJson);
            long count = a.get("count").asLong();
            double sum = a.get("sum").asDouble();
            double sumsq = a.get("sumsq").asDouble();
            double last = a.get("last").asDouble();

            double mean = sum / count;
            double variance = Math.max(0.0, sumsq / count - mean * mean);
            double std = Math.sqrt(variance);
            // Need at least a couple of points and real spread before flagging.
            boolean anomaly = count >= 2 && std > 0 && last > mean + 2 * std;

            upsert(merchant, round2(sum), round2(mean), count, anomaly);
            System.out.printf("agg  %-22s count=%-4d revenue=%-10.2f avg=%-8.2f anomaly=%s%n",
                merchant, count, sum, mean, anomaly);
        } catch (Exception e) {
            System.err.println("Failed to write aggregation for " + merchant + ": " + e.getMessage());
        }
    }

    private static synchronized void upsert(String merchant, BigDecimal revenue, BigDecimal avg,
                                            long count, boolean anomaly) throws Exception {
        String sql =
            "INSERT INTO aggregations " +
            "(merchant, total_revenue, avg_amount, transaction_count, anomaly_flag, updated_at) " +
            "VALUES (?, ?, ?, ?, ?, now()) " +
            "ON CONFLICT (merchant) DO UPDATE SET " +
            "total_revenue = EXCLUDED.total_revenue, " +
            "avg_amount = EXCLUDED.avg_amount, " +
            "transaction_count = EXCLUDED.transaction_count, " +
            "anomaly_flag = EXCLUDED.anomaly_flag, " +
            "updated_at = EXCLUDED.updated_at";
        try (PreparedStatement ps = conn.prepareStatement(sql)) {
            ps.setString(1, merchant);
            ps.setBigDecimal(2, revenue);
            ps.setBigDecimal(3, avg);
            ps.setLong(4, count);
            ps.setBoolean(5, anomaly);
            ps.executeUpdate();
        }
    }

    private static String merchantOf(String txnJson) {
        try {
            return MAPPER.readTree(txnJson).get("merchant").asText();
        } catch (Exception e) {
            return "unknown";
        }
    }

    /** Turn a libpq URL (postgresql://user:pass@host:port/db) into a JDBC connection. */
    private static Connection connect(String dbUrl) throws Exception {
        URI uri = new URI(dbUrl);
        String user = "postgres", password = "postgres";
        if (uri.getUserInfo() != null) {
            String[] parts = uri.getUserInfo().split(":", 2);
            user = parts[0];
            if (parts.length > 1) password = parts[1];
        }
        int port = uri.getPort() == -1 ? 5432 : uri.getPort();
        String jdbc = "jdbc:postgresql://" + uri.getHost() + ":" + port + uri.getPath();
        return DriverManager.getConnection(jdbc, user, password);
    }

    private static BigDecimal round2(double v) {
        return BigDecimal.valueOf(v).setScale(2, RoundingMode.HALF_UP);
    }

    private static String getenv(String key, String fallback) {
        String v = System.getenv(key);
        return (v == null || v.isEmpty()) ? fallback : v;
    }

    private static void closeQuietly(Connection c) {
        try { if (c != null) c.close(); } catch (Exception ignored) {}
    }
}
