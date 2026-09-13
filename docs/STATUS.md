# Status

## Project Status: ✅ Created (initial)

| Component | File | Status |
|-----------|------|--------|
| go.mod | go.mod | ✅ Created |
| KeyValueEmbd struct + New/Close | keyvalembd.go | ✅ Created |
| Создание таблиц | keyvalembd.go | ✅ Created |
| Get/Set/Del | crud.go | ✅ Created |
| SetWithEmbedding | crud.go | ✅ Created |
| List/Count/IsFolder/Dir | list.go | ✅ Created |
| GetInfo/SetInfo | info.go | ✅ Created |
| Embedder | embedder.go | ✅ Created |
| SearchSemantic/SearchByEmbedding | search.go | ✅ Created |
| Тесты | keyvalembd_test.go | ✅ Created |
| Retry tests | embedder_test.go | ✅ Created |
| docs/CONTEXT.md | docs/CONTEXT.md | ✅ Created |
| docs/DESIGN.md | docs/DESIGN.md | ✅ Created |
| docs/STATUS.md | docs/STATUS.md | ✅ Created |
| Model() test + godoc | embedder.go, keyvalembd_test.go | ✅ Added (#2) |
| Native vector index (migration + ANN search) | vector.go | ✅ Added (v0.4.0) |
| Vector index tests | vector_test.go | ✅ Added (v0.4.0) |

## Known Issues & Fixes

| Component | File | Status |
|---|---|---|
| Fix: SetInfo RFC3339 + robust parsing | info.go, keyvalembd.go, crud.go, keyvalembd_test.go | ✅ Fixed (#1) |
| Test: Model() getter + godoc | embedder.go, keyvalembd_test.go | ✅ Fixed (#2) |
| Fix: SearchByEmbedding ignores rows.Err() and scan errors | search.go | ✅ Fixed (#3) |
| Fix: List ignores rows.Err() and scan errors | list.go | ✅ Fixed (#3) |
| Fix: Silently swallowed errors in marshal/unmarshal and error logging | crud.go, info.go, keyvalembd.go, embedder.go | ✅ Fixed (#5) |
| Vector column write falls back to raw embedding on dimension mismatch | crud.go | ✅ Added (v0.4.0) |

## Build Status

- `go build ./...` — ✅ PASS
- `go vet ./...` — ✅ PASS
- `go test ./...` — ✅ PASS (49 tests, ~0.64s, including real Ollama SearchSemantic)
- Coverage: `GenerateEmbedding` 93.2%, `retryDelay` 100%

## Native Vector Index (v0.4.0)

Semantic search now uses libSQL's DiskANN vector index when the collection
exceeds `VectorIndexConfig.Threshold` (default 1500), with exact re-ranking of
an oversampled candidate set. Smaller collections keep the exact scan.

Measured on a 6845-vector collection (768-dim): 22 ms vs 124 ms per query
(5.6x), recall@1/@5 100%, recall@10 ~98%.

Migration is explicit via `MigrateVectorIndex()` because the index build takes
about two minutes on 6845 vectors.

## Next Steps

1. Consider bumping rag-mcp (175 vectors) and ai-hub/hub-server (28 vectors) to
   v0.4.0 for consistency; both stay on the exact scan (below threshold).
2. Re-evaluate the threshold with a benchmark once collections grow.

