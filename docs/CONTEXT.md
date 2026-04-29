# Context

## Project Overview

keyvalembd is a Go library that provides an S3-like key-value store with vector (embedding) search capabilities. It implements the `KeyValueStore` interface from [s3lite](https://github.com/kirill-scherba/s3lite) but uses **libSQL** as the storage backend instead of BadgerDB, and adds **semantic search** via Ollama embeddings.

## Key Features

- S3-like key-value storage (Get, Set, Del, List, Count, folder semantics)
- libSQL (SQLite-compatible) backend with WAL mode
- Object metadata (content type, checksum, timestamps)
- Embedding generation via Ollama (embeddinggemma:latest)
- Semantic / vector search across stored values
- Implements `s3lite.KeyValueStore` interface for drop-in replacement

## Architecture

```
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

## Dependencies

- Go 1.26+
- Ollama with embedding model (embeddinggemma:latest)
- libSQL (go-libsql driver)

## Related Projects

- [s3lite](https://github.com/kirill-scherba/s3lite) — S3-like storage on BadgerDB (interface definition)
- [sqlh](https://github.com/kirill-scherba/sqlh) — SQL helper for Go generics
- [web-search-mcp](https://github.com/kirill-scherba/web-search-mcp) — MCP server with embeddings (proven implementation)
