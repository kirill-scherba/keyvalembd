// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/kirill-scherba/keyvalembd/vecindex"
)

const (
	// vectorColumnName is the column holding the embedding vector.
	vectorColumnName = "embedding_vec"
	// legacyEmbeddingColumn is the pre-v0.5.0 vector column, still read on
	// databases that have not been migrated.
	legacyEmbeddingColumn = "embedding"
	// vectorTableName is the table that holds embeddings.
	vectorTableName = "kv_embeddings"
	// defaultVectorDim is used when the database has no embeddings yet.
	defaultVectorDim = 768
)

// errIndexDisabled is returned by searchByEmbeddingIndex when no index
// directory is configured.
var errIndexDisabled = errors.New("vector index is not configured")

// SetVectorIndexDir enables the in-process, memory-mapped vector index rooted
// at dir. An empty dir disables it.
//
// The index is built lazily on the first search and rebuilt whenever the
// database changes. It is a derived artefact: the database remains the source
// of truth, so a missing or corrupt index is simply rebuilt.
func (kv *KeyValueEmbd) SetVectorIndexDir(dir string) {
	kv.vecMu.Lock()
	defer kv.vecMu.Unlock()

	if kv.idxDir == dir {
		return
	}
	if kv.idx != nil {
		if err := kv.idx.Close(); err != nil {
			log.Printf("keyvalembd: close vector index: %v", err)
		}
		kv.idx = nil
	}
	kv.idxDir = dir
	kv.idxDirty = dir != ""
}

// VectorIndexDir returns the configured index directory ("" when disabled).
func (kv *KeyValueEmbd) VectorIndexDir() string {
	kv.vecMu.RLock()
	defer kv.vecMu.RUnlock()
	return kv.idxDir
}

// VectorIndexReady reports whether an index is currently open and up to date.
func (kv *KeyValueEmbd) VectorIndexReady() bool {
	kv.vecMu.RLock()
	defer kv.vecMu.RUnlock()
	return kv.idx != nil && !kv.idxDirty
}

// RebuildVectorIndex forces a rebuild of the in-process index from the
// database. It is a no-op when no index directory is configured.
func (kv *KeyValueEmbd) RebuildVectorIndex() error {
	kv.vecMu.Lock()
	defer kv.vecMu.Unlock()
	if kv.idxDir == "" {
		return errIndexDisabled
	}
	return kv.buildIndexLocked()
}

// markIndexDirty records that the database changed and the index must be
// rebuilt before the next search.
func (kv *KeyValueEmbd) markIndexDirty() {
	kv.vecMu.Lock()
	if kv.idxDir != "" {
		kv.idxDirty = true
	}
	kv.vecMu.Unlock()
}

// searchByEmbeddingIndex answers a query from the in-process index, rebuilding
// it first when the database has changed.
func (kv *KeyValueEmbd) searchByEmbeddingIndex(embedding []float32, limit int) ([]SearchResult, error) {
	// Fast path: index present and current.
	kv.vecMu.RLock()
	if kv.idxDir == "" {
		kv.vecMu.RUnlock()
		return nil, errIndexDisabled
	}
	if kv.idx != nil && !kv.idxDirty {
		hits := kv.idx.Search(embedding, limit)
		kv.vecMu.RUnlock()
		return kv.resultsWithText(hitsToResults(hits)), nil
	}
	kv.vecMu.RUnlock()

	// Slow path: (re)build, then search.
	kv.vecMu.Lock()
	defer kv.vecMu.Unlock()
	if kv.idx == nil || kv.idxDirty {
		if err := kv.buildIndexLocked(); err != nil {
			return nil, err
		}
	}
	hits := kv.idx.Search(embedding, limit)
	return kv.resultsWithText(hitsToResults(hits)), nil
}

// buildIndexLocked rebuilds the on-disk index from the database and reopens it.
// The caller must hold vecMu for writing.
func (kv *KeyValueEmbd) buildIndexLocked() error {
	if kv.idx != nil {
		if err := kv.idx.Close(); err != nil {
			log.Printf("keyvalembd: close vector index: %v", err)
		}
		kv.idx = nil
	}

	dim, err := kv.detectEmbeddingDim(kv.vectorColumnLocked())
	if err != nil {
		return fmt.Errorf("detect embedding dimension: %w", err)
	}

	col := kv.vectorColumnLocked()
	rows, err := kv.db.Query(fmt.Sprintf(
		`SELECT key, %s FROM %s WHERE %s IS NOT NULL`,
		col, vectorTableName, col,
	))
	if err != nil {
		return fmt.Errorf("read embeddings: %w", err)
	}
	defer rows.Close()

	next := func() (string, []float32, bool, error) {
		if !rows.Next() {
			return "", nil, false, rows.Err()
		}
		var (
			key  string
			blob []byte
		)
		if err := rows.Scan(&key, &blob); err != nil {
			return "", nil, false, err
		}
		return key, bytesToFloat32Slice(blob), true, nil
	}

	ix, err := vecindex.Build(kv.idxDir, dim, next)
	if err != nil {
		return fmt.Errorf("build vector index: %w", err)
	}

	kv.idx = ix
	kv.idxDirty = false
	log.Printf("keyvalembd: vector index ready: %d vectors, %.1f MB",
		ix.Count(), float64(ix.DiskBytes())/(1<<20))
	return nil
}

// resultsWithText fills SearchResult.Text from kv_embeddings. The in-process
// index stores only key and vector; the text that produced the embedding lives
// in the database. This is one small indexed lookup for the top-K keys, not a
// scan.
func (kv *KeyValueEmbd) resultsWithText(results []SearchResult) []SearchResult {
	if len(results) == 0 {
		return results
	}

	args := make([]any, len(results))
	placeholders := make([]byte, 0, len(results)*2)
	for i, r := range results {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args[i] = r.Key
	}

	rows, err := kv.db.Query(
		`SELECT key, text FROM `+vectorTableName+
			` WHERE key IN (`+string(placeholders)+`)`, args...)
	if err != nil {
		log.Printf("keyvalembd: read result text: %v", err)
		return results
	}
	defer rows.Close()

	texts := make(map[string]string, len(results))
	for rows.Next() {
		var key, text string
		if err := rows.Scan(&key, &text); err != nil {
			log.Printf("keyvalembd: scan result text: %v", err)
			continue
		}
		texts[key] = text
	}
	if err := rows.Err(); err != nil {
		log.Printf("keyvalembd: iterate result text: %v", err)
	}

	for i := range results {
		results[i].Text = texts[results[i].Key]
	}
	return results
}

// hitsToResults converts index hits to SearchResults.
func hitsToResults(hits []vecindex.Hit) []SearchResult {
	out := make([]SearchResult, len(hits))
	for i, h := range hits {
		out[i] = SearchResult{Key: h.Key, Score: h.Score}
	}
	return out
}

// vectorColumn returns the column that currently holds the embedding vector:
// embedding_vec when present, the legacy embedding column otherwise.
func (kv *KeyValueEmbd) vectorColumn() string {
	kv.vecMu.RLock()
	defer kv.vecMu.RUnlock()
	return kv.vectorColumnLocked()
}

// vectorColumnLocked is vectorColumn for callers that already hold vecMu.
func (kv *KeyValueEmbd) vectorColumnLocked() string {
	if kv.columnOK {
		return vectorColumnName
	}
	return legacyEmbeddingColumn
}

// refreshVectorState detects which vector column the database has.
func (kv *KeyValueEmbd) refreshVectorState() {
	ok, err := kv.columnExists(vectorTableName, vectorColumnName)
	if err != nil {
		log.Printf("keyvalembd: detect vector column: %v", err)
	}
	kv.vecMu.Lock()
	kv.columnOK = ok
	kv.vecMu.Unlock()
}

// columnExists reports whether a column is present in a table.
func (kv *KeyValueEmbd) columnExists(table, column string) (bool, error) {
	rows, err := kv.db.Query(fmt.Sprintf("PRAGMA table_info(%q)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// detectEmbeddingDim infers the embedding dimension from stored data. It
// returns defaultVectorDim when there are no embeddings yet. col is passed in
// because callers may already hold vecMu.
func (kv *KeyValueEmbd) detectEmbeddingDim(col string) (int, error) {
	var length int
	err := kv.db.QueryRow(fmt.Sprintf(
		`SELECT length(%s) FROM %s WHERE %s IS NOT NULL LIMIT 1`,
		col, vectorTableName, col,
	)).Scan(&length)
	if err == sql.ErrNoRows || length <= 0 {
		return defaultVectorDim, nil
	}
	if err != nil {
		return 0, err
	}
	return length / 4, nil
}

// DropLegacyEmbeddingColumn removes the redundant legacy embedding BLOB column
// once embedding_vec exists. It is a no-op when the legacy column is already
// absent, and it refuses to act when embedding_vec is missing or not fully
// backfilled, so it can never drop the only copy of the vectors.
func (kv *KeyValueEmbd) DropLegacyEmbeddingColumn() (bool, error) {
	if !kv.enabled {
		return false, fmt.Errorf("keyvalembd is not enabled")
	}

	legacyOK, err := kv.columnExists(vectorTableName, legacyEmbeddingColumn)
	if err != nil {
		return false, fmt.Errorf("check legacy column: %w", err)
	}
	if !legacyOK {
		return false, nil
	}

	vecOK, err := kv.columnExists(vectorTableName, vectorColumnName)
	if err != nil {
		return false, fmt.Errorf("check vector column: %w", err)
	}
	if !vecOK {
		return false, fmt.Errorf("refusing to drop %s: %s column is missing",
			legacyEmbeddingColumn, vectorColumnName)
	}

	var missing int
	if err := kv.db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL AND %s IS NULL`,
		vectorTableName, legacyEmbeddingColumn, vectorColumnName,
	)).Scan(&missing); err != nil {
		return false, fmt.Errorf("check backfill: %w", err)
	}
	if missing > 0 {
		return false, fmt.Errorf("refusing to drop %s: %d rows not backfilled into %s",
			legacyEmbeddingColumn, missing, vectorColumnName)
	}

	if _, err := kv.db.Exec(fmt.Sprintf(
		`ALTER TABLE %s DROP COLUMN %s`, vectorTableName, legacyEmbeddingColumn,
	)); err != nil {
		return false, fmt.Errorf("drop legacy column: %w", err)
	}

	kv.refreshVectorState()
	return true, nil
}
