# Context

## Project Overview

keyvalembd is a Go library that provides an S3-like key-value store with vector (embedding) search capabilities. It implements the `KeyValueStore` interface from [s3lite](https://github.com/kirill-scherba/s3lite) but uses a **pure-Go SQLite** driver ([modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)) as the storage backend instead of BadgerDB, and adds **semantic search** via Ollama embeddings plus an in-process memory-mapped vector index.

## Key Features

- S3-like key-value storage (Get, Set, Del, List, Count, folder semantics)
- Pure-Go SQLite backend (modernc.org/sqlite) with WAL mode and foreign keys; builds with `CGO_ENABLED=0`
- Object metadata (content type, checksum, timestamps)
- Embedding generation via Ollama (embeddinggemma:latest)
- Semantic / vector search across stored values
- Optional native libSQL vector index (DiskANN) for large collections, with
  automatic fallback to the exact cosine scan
- Implements `s3lite.KeyValueStore` interface for drop-in replacement

## Architecture

```
┌──────────────────────────────────────────────┐
│            keyvalembd.KeyValueEmbd            │
│  implements s3lite.KeyValueStore             │
│                                               │
│  ┌────────────────┐  ┌──────────────────┐    │
│  │  SQLite Store   │  │  Ollama Embedder  │   │
│  │  ┌───────────┐  │  │  ┌─────────────┐ │    │
│  │  │ kv_data   │  │  │  │ GenerateEmb │ │    │
│  │  │ vecindex  │  │  │  │ cosineSim   │ │    │
│  │  │ kv_embdgs │  │  │  │ cosineSim   │ │    │
│  │  └───────────┘  │  │  └─────────────┘ │    │
│  └────────────────┘  └──────────────────┘    │
└──────────────────────────────────────────────┘
```

## Dependencies

- Go 1.26+
- Ollama with embedding model (embeddinggemma:latest)
- modernc.org/sqlite (pure Go, no CGO)

## Vector Index

Semantic search is exact. By default it scans the embeddings in the database;
when a directory is configured with `SetVectorIndexDir` it is answered from an
in-process, memory-mapped index instead:

```
meta.json    version, dim, count, built_at
vectors.bin  count × dim float32, little-endian
keys.bin     (count+1) × uint64 offsets, followed by concatenated key bytes
```

Both data files are mapped read-only, so the kernel pages in what is touched and
can reclaim it; the collection never sits on the Go heap. Measured on 6848 x 768:
build 74 ms, disk 20.4 MB (3125 B/vector), search 5 ms, recall 100%. The libSQL
DiskANN index this replaced needed 118 s to build, occupied 1050 MB and answered
in 22 ms at ~98% recall.

## Related Projects

- [s3lite](https://github.com/kirill-scherba/s3lite) — S3-like storage on BadgerDB (interface definition)
- [sqlh](https://github.com/kirill-scherba/sqlh) — SQL helper for Go generics
- [web-search-mcp](https://github.com/kirill-scherba/web-search-mcp) — MCP server with embeddings (proven implementation)
