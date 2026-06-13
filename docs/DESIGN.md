# Design & Architecture

## Why libSQL + Embeddings?

- **libSQL** provides SQL semantics, allowing rich metadata queries alongside key-value storage
- **Ollama embeddings** enable semantic search without external vector database dependencies
- **s3lite interface** ensures compatibility with existing code that uses S3-like storage

## Database Schema

```sql
-- Primary key-value + metadata
CREATE TABLE kv_data (
    key          TEXT PRIMARY KEY NOT NULL,
    value        BLOB NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    checksum     TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    modified_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    metadata     TEXT NOT NULL DEFAULT '{}'
);

-- Embeddings for semantic search
CREATE TABLE kv_embeddings (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    key        TEXT NOT NULL UNIQUE REFERENCES kv_data(key) ON DELETE CASCADE,
    text       TEXT NOT NULL DEFAULT '',
    embedding  BLOB,           -- []float32 → 4 bytes per float, little-endian
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
```

Embedding dimension: 768 (embeddinggemma model).

**Timestamp format:** All `created_at` and `modified_at` columns use RFC3339
(`"2006-01-02T15:04:05Z"`). Legacy databases may contain SQLite's `datetime('now')`
format (`"2006-01-02 15:04:05"`); `parseTimestamp()` handles both transparently.

## Folder Semantics (S3-like)

libSQL is not hierarchical, so folder support is implemented at the application level, identical to s3lite:

1. A key ending with `/` is a folder
2. `List(prefix)` fetches all keys matching `prefix || '%'` via SQL LIKE
3. Keys deeper than prefix depth are collapsed into folder entries (e.g., `a/b/c` → `a/b/` when listing at prefix `a/`)
4. Deduplication via map ensures each sub-folder appears only once

## Similarity Search

Cosine similarity is computed in Go (not SQL):

```go
func cosineSimilarity(a, b []float32) float64 {
    dotProduct += float64(a[i]) * float64(b[i])
    normA += float64(a[i]) * float64(a[i])
    normB += float64(b[i]) * float64(b[i])
    return dotProduct / (sqrt(normA) * sqrt(normB))
}
```

All stored embeddings are fetched and compared in Go. For large collections, SQL-level vector search via libsql vector extension can be added later.

## Embedder

The embedder communicates with Ollama via REST API:

- `POST /api/embeddings` with `{model, prompt}` returns `{embedding: []float32}`
- Retry logic with exponential backoff (3 attempts)
- Connection pooling (10 idle, 90s timeout)
- Default model: `embeddinggemma:latest`

## Extended Interface (beyond s3lite)

```go
// Additional methods on KeyValueEmbd:
SetWithEmbedding(key string, value []byte, text string) (*ObjectInfo, error)
SearchSemantic(query string, limit int) ([]SearchResult, error)
SearchByEmbedding(embedding []float32, limit int) ([]SearchResult, error)
```

These methods provide vector search functionality on top of the standard S3-like interface.
