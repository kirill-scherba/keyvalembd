// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"fmt"
	"log"
	"sort"
)

// SearchResult represents a single result from a semantic search.
type SearchResult struct {
	Key   string  `json:"key"`
	Score float64 `json:"score"`
	Text  string  `json:"text"`
}

// SearchSemantic generates an embedding for the query text and performs a
// cosine similarity search across all stored embeddings, returning the top-N
// results. The embedder must be ready (Ollama available).
func (kv *KeyValueEmbd) SearchSemantic(query string, limit int) ([]SearchResult, error) {
	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}
	if kv.embedder == nil || !kv.embedder.Ready() {
		return nil, fmt.Errorf("embedder is not ready")
	}
	if limit <= 0 {
		limit = 10
	}

	// Generate query embedding
	queryEmb, err := kv.embedder.GenerateEmbedding(query)
	if err != nil {
		return nil, fmt.Errorf("generate embedding: %w", err)
	}

	return kv.SearchByEmbedding(queryEmb, limit)
}

// SearchByEmbedding performs a cosine similarity search using the given
// embedding vector, returning the top-N results.
//
// When the native libSQL vector index is available and the collection is large
// enough, the search is delegated to the index with exact re-ranking of the
// candidate set. Otherwise an exact scan of all stored embeddings is used.
func (kv *KeyValueEmbd) SearchByEmbedding(embedding []float32, limit int) ([]SearchResult, error) {
	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}
	if limit <= 0 {
		limit = 10
	}

	if kv.useVectorIndex() {
		results, err := kv.searchByEmbeddingANN(embedding, limit)
		if err == nil {
			return results, nil
		}
		// Non-fatal: fall back to the exact scan.
		log.Printf("keyvalembd: vector index search failed, using scan: %v", err)
	}

	return kv.searchByEmbeddingScan(embedding, limit)
}

// searchByEmbeddingScan performs an exact cosine similarity scan over every
// stored embedding. It is O(N) and is used for small collections or when the
// native vector index is unavailable.
//
// The vector column is resolved at runtime (embedding_vec, or the legacy
// embedding column on databases that have not been migrated), which is why the
// query is raw SQL rather than sqlh's struct mapping.
func (kv *KeyValueEmbd) searchByEmbeddingScan(embedding []float32, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}

	col := kv.vectorColumn()
	query := fmt.Sprintf(
		`SELECT key, text, %s FROM %s WHERE %s IS NOT NULL`,
		col, vectorTableName, col,
	)

	rows, err := kv.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("scan embeddings: %w", err)
	}
	defer rows.Close()

	type scored struct {
		key   string
		text  string
		score float64
	}

	var scoredResults []scored
	for rows.Next() {
		var (
			key, text string
			blob      []byte
		)
		if err := rows.Scan(&key, &text, &blob); err != nil {
			log.Printf("keyvalembd: SearchByEmbedding: scan row: %v", err)
			continue
		}
		scoredResults = append(scoredResults, scored{
			key:   key,
			text:  text,
			score: cosineSimilarity(embedding, bytesToFloat32Slice(blob)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate embeddings: %w", err)
	}

	// Sort by score descending
	sort.Slice(scoredResults, func(i, j int) bool {
		return scoredResults[i].score > scoredResults[j].score
	})

	// Limit results
	if len(scoredResults) > limit {
		scoredResults = scoredResults[:limit]
	}

	// Convert to SearchResult
	results := make([]SearchResult, len(scoredResults))
	for i, sr := range scoredResults {
		results[i] = SearchResult{
			Key:   sr.key,
			Score: sr.score,
			Text:  sr.text,
		}
	}

	return results, nil
}
