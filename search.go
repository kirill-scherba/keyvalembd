// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/kirill-scherba/sqlh"
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

// kvEmbeddingRow is a lightweight projection of KVEmbedding used by
// SearchByEmbedding to load only key, text and embedding blob.
type kvEmbeddingRow struct {
	_         bool   `db_table_name:"kv_embeddings"`
	Key       string `db:"key"`
	Text      string `db:"text"`
	Embedding []byte `db:"embedding" db_type:"BLOB"`
}

// SearchByEmbedding performs a cosine similarity search using the given
// embedding vector against all stored embeddings, returning the top-N results.
func (kv *KeyValueEmbd) SearchByEmbedding(embedding []float32, limit int) ([]SearchResult, error) {
	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}
	if limit <= 0 {
		limit = 10
	}

	type scored struct {
		key   string
		text  string
		score float64
	}

	var scoredResults []scored

	for _, row := range sqlh.ListRange[kvEmbeddingRow](
		kv.db, 0, "", "", 0,
		sqlh.IsNotNull("embedding"),
		func(err error) { log.Printf("keyvalembd: SearchByEmbedding: iterate: %v", err) },
		context.Background(),
	) {
		storedEmb := bytesToFloat32Slice(row.Embedding)
		score := cosineSimilarity(embedding, storedEmb)
		scoredResults = append(scoredResults, scored{
			key:   row.Key,
			text:  row.Text,
			score: score,
		})
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
