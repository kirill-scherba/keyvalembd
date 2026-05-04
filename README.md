# keyvalembd

[![Go Version](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go)](https://go.dev/)
[![License](https://img.shields.io/badge/License-BSD%203--Clause-blue.svg)](LICENSE)

**keyvalembd** is a Go library that provides an S3-like key-value store with vector (embedding) search. It uses [libSQL](https://github.com/tursodatabase/libsql) (SQLite-compatible) as the storage backend and optionally generates embeddings via [Ollama](https://ollama.ai/) for semantic search.

The library implements the [`s3lite.KeyValueStore`](https://github.com/kirill-scherba/s3lite) interface, enabling drop-in replacement for code that uses S3-style storage.

---

## Features

- **S3-like key-value storage** — `Get`, `Set`, `Del`, `List`, `Count` with folder semantics
- **libSQL backend** — WAL mode, concurrent access, SQL metadata queries
- **Object metadata** — content type, MD5 checksum, timestamps, custom key-value metadata
- **Embedding generation** — automatic vector embeddings via Ollama
- **Semantic search** — cosine similarity search across stored embeddings
- **Drop-in compatible** — implements `s3lite.KeyValueStore` interface
- **Folder hierarchy** — S3-style prefix listing with sub-folder collapsing

---

## Installation

```bash
go get github.com/kirill-scherba/keyvalembd
```

Requires Go 1.26+ and an Ollama instance with an embedding model (default: `embeddinggemma:latest`).

---

## Quick Start

```go
package main

import (
    "fmt"
    "log"

    "github.com/kirill-scherba/keyvalembd"
)

func main() {
    // Create a new key-value store (auto-creates temp dir if path is empty)
    kv, err := keyvalembd.New("/tmp/my-kv.db")
    if err != nil {
        log.Fatal(err)
    }
    defer kv.Close()

    // Set a value
    _, err = kv.Set("greeting", []byte("Hello, World!"))
    if err != nil {
        log.Fatal(err)
    }

    // Get a value
    data, err := kv.Get("greeting")
    if err != nil {
        log.Fatal(err)
    }
    fmt.Println(string(data)) // Output: Hello, World!

    // Set with metadata
    _, err = kv.Set("doc.md", []byte("# Document"),
        s3lite.SetContentType("text/markdown"))
    if err != nil {
        log.Fatal(err)
    }

    // Set with embedding for semantic search
    _, err = kv.SetWithEmbedding("note", []byte("AI note"),
        "Artificial intelligence and machine learning concepts")
    if err != nil {
        log.Fatal(err)
    }

    // Semantic search (requires Ollama with embedding model)
    results, err := kv.SearchSemantic("machine learning", 5)
    if err != nil {
        log.Printf("Semantic search unavailable: %v", err)
    } else {
        for _, r := range results {
            fmt.Printf("  %s (score: %.3f): %s\n", r.Key, r.Score, r.Text)
        }
    }

    // List keys with prefix
    for key := range kv.List("") {
        fmt.Println(key)
    }

    // Delete a key
    err = kv.Del("greeting")
    if err != nil {
        log.Fatal(err)
    }
}
```

---

## API

### Core Operations

| Method | Description |
| -------- | ------------- |
| `New(dbPath string) (*KeyValueEmbd, error)` | Create or open a key-value store |
| `Close()` | Close the database connection |
| `Get(key string) ([]byte, error)` | Retrieve a value by key |
| `Set(key string, value []byte, info ...*s3lite.ObjectInfo) (*s3lite.ObjectInfo, error)` | Set a key-value pair |
| `Del(keys ...string) error` | Delete one or more keys |

### Metadata

| Method | Description |
| -------- | ------------- |
| `GetInfo(key string) (*s3lite.ObjectInfo, error)` | Retrieve object metadata |
| `SetInfo(key string, info *s3lite.ObjectInfo) (*s3lite.ObjectInfo, error)` | Update object metadata |

### Listing & Navigation

| Method | Description |
| -------- | ------------- |
| `List(prefix string) iter.Seq[string]` | Iterate over keys with prefix (S3 folder semantics) |
| `Count(prefix string) int` | Count keys with prefix |
| `IsFolder(key string) bool` | Check if key is a folder (ends with `/`) |
| `IsFolderWithFiles(key string) bool` | Check if folder contains files |
| `Dir(key string) string` | Return directory part of a key |

### Embedding & Search

| Method | Description |
| -------- | ------------- |
| `SetWithEmbedding(key string, value []byte, text string, info ...*s3lite.ObjectInfo) (*s3lite.ObjectInfo, error)` | Store a value with an embedding generated from text |
| `SearchSemantic(query string, limit int) ([]SearchResult, error)` | Search by semantic similarity (text query → embedding → cosine similarity) |
| `SearchByEmbedding(embedding []float32, limit int) ([]SearchResult, error)` | Search by raw embedding vector |

### SearchResult

```go
type SearchResult struct {
    Key   string  `json:"key"`
    Score float64 `json:"score"`
    Text  string  `json:"text"`
}
```

---

## Architecture

```txt
┌──────────────────────────────────────────────┐
│            keyvalembd.KeyValueEmbd            │
│  implements s3lite.KeyValueStore             │
│                                               │
│  ┌────────────────┐  ┌──────────────────┐    │
│  │  libSQL Store   │  │  Ollama Embedder  │   │
│  │  ┌───────────┐  │  │  ┌─────────────┐ │    │
│  │  │ kv_data   │  │  │  │ GenerateEmb │ │    │
│  │  │ kv_embdgs │  │  │  │ cosineSim   │ │    │
│  │  └───────────┘  │  │  └─────────────┘ │    │
│  └────────────────┘  └──────────────────┘    │
└──────────────────────────────────────────────┘
```

### Database Schema

```sql
-- Primary key-value + metadata
CREATE TABLE kv_data (
    key          TEXT PRIMARY KEY NOT NULL,
    value        BLOB NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    checksum     TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    modified_at  TEXT NOT NULL DEFAULT (datetime('now')),
    metadata     TEXT NOT NULL DEFAULT '{}'
);

-- Embeddings for semantic search
CREATE TABLE kv_embeddings (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    key        TEXT NOT NULL UNIQUE REFERENCES kv_data(key) ON DELETE CASCADE,
    text       TEXT NOT NULL DEFAULT '',
    embedding  BLOB,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

Embedding dimension: 768 (`embeddinggemma` model), stored as little-endian `[]float32` (4 bytes per float).

### Embedder

- Communicates with Ollama via REST API (`POST /api/embeddings`)
- Retry logic with exponential backoff (3 attempts)
- Connection pooling (10 idle connections, 90s timeout)
- Default model: [`embeddinggemma:latest`](https://ollama.com/library/embeddinggemma)
- Gracefully degrades if Ollama is unavailable

### Folder Semantics

libSQL is not hierarchical, so folder support is implemented at the application level (same as s3lite):

1. A key ending with `/` is a folder
2. `List(prefix)` fetches all keys matching `prefix || '%'` via SQL LIKE
3. Keys deeper than prefix depth are collapsed into folder entries (e.g., `a/b/c` → `a/b/`)
4. Deduplication ensures each sub-folder appears only once

---

## Requirements

- Go 1.26+
- Ollama with an embedding model (for semantic search; optional)

---

## Related Projects

- [s3lite](https://github.com/kirill-scherba/s3lite) — S3-like storage on BadgerDB (defines the `KeyValueStore` interface)
- [memory-store-mcp](https://github.com/kirill-scherba/memory-store-mcp) — MCP server with persistent memory and semantic search (uses keyvalembd)
- [rag-mcp](https://github.com/kirill-scherba/rag-mcp) — RAG-based MCP server (uses keyvalembd)
- [sqlh](https://github.com/kirill-scherba/sqlh) — SQL helper for Go generics

---

## License

BSD 3-Clause License. See [LICENSE](LICENSE).
