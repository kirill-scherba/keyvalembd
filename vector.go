// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Native libSQL vector index support.
//
// libSQL ships a vector extension that provides an approximate nearest
// neighbour index (DiskANN) over F32_BLOB columns. When enabled and the
// collection is large enough, semantic search is delegated to that index
// instead of scanning every stored embedding in Go.
//
// The index is maintained by libSQL automatically on INSERT/UPDATE/DELETE,
// so the write path only has to keep the vector column populated.

const (
	// vectorColumnName is the F32_BLOB column holding the indexed vector.
	vectorColumnName = "embedding_vec"
	// legacyEmbeddingColumn is the pre-v0.5.0 vector column, kept only for
	// databases that have not been migrated yet.
	legacyEmbeddingColumn = "embedding"
	// vectorIndexName is the name of the DiskANN index over vectorColumnName.
	vectorIndexName = "kv_embeddings_vec_idx"
	// vectorTableName is the table that holds embeddings.
	vectorTableName = "kv_embeddings"
	// defaultVectorDim is used when the database has no embeddings yet and the
	// dimension cannot be inferred from existing data.
	defaultVectorDim = 768
	// vectorCountTTL bounds how long the cached embedding count is trusted.
	// The count only gates the size threshold, so a stale value is harmless.
	vectorCountTTL = 10 * time.Second
)

// VectorIndexConfig controls when and how the native libSQL vector index is
// used for semantic search.
type VectorIndexConfig struct {
	// Enabled allows the index to be used at all. When false, semantic search
	// always falls back to the exact in-Go scan.
	Enabled bool
	// Threshold is the minimum number of stored embeddings required before the
	// index is used. Below it the exact scan is both faster and exact.
	Threshold int
	// Oversample is the factor by which the requested limit is multiplied when
	// fetching ANN candidates. Candidates are then re-ranked by exact cosine
	// distance, which restores recall to ~100%.
	Oversample int
}

// DefaultVectorIndexConfig returns the recommended defaults: the index is
// enabled, used from 1500 embeddings up, with 3x candidate oversampling.
func DefaultVectorIndexConfig() VectorIndexConfig {
	return VectorIndexConfig{
		Enabled:    true,
		Threshold:  1500,
		Oversample: 3,
	}
}

// SetVectorIndexConfig replaces the vector index configuration. Zero values
// are normalised: Threshold below zero becomes zero, Oversample below one
// becomes one.
func (kv *KeyValueEmbd) SetVectorIndexConfig(cfg VectorIndexConfig) {
	if cfg.Threshold < 0 {
		cfg.Threshold = 0
	}
	if cfg.Oversample < 1 {
		cfg.Oversample = 1
	}
	kv.vecMu.Lock()
	kv.vecCfg = cfg
	kv.vecMu.Unlock()
}

// VectorIndexConfig returns a copy of the current vector index configuration.
func (kv *KeyValueEmbd) VectorIndexConfig() VectorIndexConfig {
	kv.vecMu.RLock()
	defer kv.vecMu.RUnlock()
	return kv.vecCfg
}

// VectorIndexReady reports whether the vector column and index both exist and
// the index is allowed by the configuration. It does not consider the size
// threshold.
func (kv *KeyValueEmbd) VectorIndexReady() bool {
	kv.vecMu.RLock()
	defer kv.vecMu.RUnlock()
	return kv.vecCfg.Enabled && kv.vecColumnOK && kv.vecIndexOK
}

// vectorColumn returns the column that currently holds the embedding vector:
// embedding_vec on migrated (or freshly created) databases, the legacy
// embedding column otherwise.
func (kv *KeyValueEmbd) vectorColumn() string {
	kv.vecMu.RLock()
	defer kv.vecMu.RUnlock()
	if kv.vecColumnOK {
		return vectorColumnName
	}
	return legacyEmbeddingColumn
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

// indexExists reports whether an index with the given name exists.
func (kv *KeyValueEmbd) indexExists(name string) (bool, error) {
	var n int
	err := kv.db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`,
		name,
	).Scan(&n)
	return n > 0, err
}

// refreshVectorIndexState re-detects whether the vector column and index are
// present and caches the result.
func (kv *KeyValueEmbd) refreshVectorIndexState() {
	columnOK, err := kv.columnExists(vectorTableName, vectorColumnName)
	if err != nil {
		log.Printf("keyvalembd: detect vector column: %v", err)
	}
	indexOK, err := kv.indexExists(vectorIndexName)
	if err != nil {
		log.Printf("keyvalembd: detect vector index: %v", err)
	}

	// Publish the column state first: countEmbeddings resolves the column via
	// vectorColumn(), so it must not run before vecColumnOK is up to date.
	kv.vecMu.Lock()
	kv.vecColumnOK = columnOK
	kv.vecIndexOK = indexOK
	kv.vecMu.Unlock()

	count := 0
	if columnOK {
		if n, err := kv.countEmbeddings(); err != nil {
			log.Printf("keyvalembd: count embeddings: %v", err)
		} else {
			count = n
		}
	}

	kv.vecMu.Lock()
	kv.vecCount = count
	kv.vecCountAt = time.Now()
	kv.vecMu.Unlock()
}

// detectEmbeddingDim infers the embedding dimension from stored data. It
// returns defaultVectorDim when there are no embeddings yet.
func (kv *KeyValueEmbd) detectEmbeddingDim() int {
	col := kv.vectorColumn()
	var length int
	err := kv.db.QueryRow(fmt.Sprintf(
		`SELECT length(%s) FROM %s WHERE %s IS NOT NULL LIMIT 1`,
		col, vectorTableName, col,
	)).Scan(&length)
	if err != nil || length <= 0 {
		return defaultVectorDim
	}
	return length / 4
}

// MigrateVectorIndex prepares the database for native vector search:
//
//  1. adds the embedding_vec F32_BLOB column if missing,
//  2. backfills it from the existing embedding column,
//  3. creates the DiskANN index if missing.
//
// The operation is idempotent and safe to call on every start. Building the
// index can take a while on large collections, so callers that care about
// startup latency should run it as an explicit migration step rather than
// inside New.
func (kv *KeyValueEmbd) MigrateVectorIndex() error {
	if !kv.enabled {
		return fmt.Errorf("keyvalembd is not enabled")
	}

	kv.vecMigrateMu.Lock()
	defer kv.vecMigrateMu.Unlock()

	// 1. Add the vector column when missing.
	columnOK, err := kv.columnExists(vectorTableName, vectorColumnName)
	if err != nil {
		return fmt.Errorf("check vector column: %w", err)
	}
	if !columnOK {
		dim := kv.detectEmbeddingDim()
		stmt := fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN %s F32_BLOB(%d)`,
			vectorTableName, vectorColumnName, dim,
		)
		if _, err := kv.db.Exec(stmt); err != nil {
			return fmt.Errorf("add vector column: %w", err)
		}
		log.Printf("keyvalembd: added %s F32_BLOB(%d) column",
			vectorColumnName, dim)
	}

	// 2. Backfill the vector column from the legacy embedding blob when that
	// column exists. libSQL accepts the little-endian float32 layout as-is, so
	// no re-encoding is needed.
	legacyOK, err := kv.columnExists(vectorTableName, legacyEmbeddingColumn)
	if err != nil {
		return fmt.Errorf("check legacy embedding column: %w", err)
	}
	if legacyOK {
		res, err := kv.db.Exec(fmt.Sprintf(
			`UPDATE %s SET %s = %s
			 WHERE %s IS NOT NULL AND %s IS NULL`,
			vectorTableName, vectorColumnName, legacyEmbeddingColumn,
			legacyEmbeddingColumn, vectorColumnName,
		))
		if err != nil {
			return fmt.Errorf("backfill vector column: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			log.Printf("keyvalembd: backfilled %d embeddings into %s",
				n, vectorColumnName)
		}
	}

	// 3. Create the index when missing.
	indexOK, err := kv.indexExists(vectorIndexName)
	if err != nil {
		return fmt.Errorf("check vector index: %w", err)
	}
	if !indexOK {
		log.Printf("keyvalembd: building vector index %s (may take a while)...",
			vectorIndexName)
		stmt := fmt.Sprintf(
			`CREATE INDEX IF NOT EXISTS %s ON %s (libsql_vector_idx(%s))`,
			vectorIndexName, vectorTableName, vectorColumnName,
		)
		if _, err := kv.db.Exec(stmt); err != nil {
			return fmt.Errorf("create vector index: %w", err)
		}
		log.Printf("keyvalembd: vector index %s ready", vectorIndexName)
	}

	kv.refreshVectorIndexState()
	return nil
}

// countEmbeddings returns the number of non-null embeddings.
func (kv *KeyValueEmbd) countEmbeddings() (int, error) {
	col := kv.vectorColumn()
	var n int
	err := kv.db.QueryRow(fmt.Sprintf(
		`SELECT COUNT(*) FROM %s WHERE %s IS NOT NULL`,
		vectorTableName, col,
	)).Scan(&n)
	return n, err
}

// DropLegacyEmbeddingColumn removes the redundant legacy embedding BLOB column
// once embedding_vec exists. After this, embedding_vec is the single source of
// truth for vectors.
//
// It is a no-op when the legacy column is already absent, and it refuses to
// act when embedding_vec is missing or not fully backfilled, so it can never
// drop the only copy of the vectors.
func (kv *KeyValueEmbd) DropLegacyEmbeddingColumn() (bool, error) {
	if !kv.enabled {
		return false, fmt.Errorf("keyvalembd is not enabled")
	}

	kv.vecMigrateMu.Lock()
	defer kv.vecMigrateMu.Unlock()

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

	kv.refreshVectorIndexState()
	return true, nil
}

// useVectorIndex reports whether semantic search should use the native index
// for the current collection size. The embedding count is cached for
// vectorCountTTL so that the hot search path does not run COUNT(*) per query.
func (kv *KeyValueEmbd) useVectorIndex() bool {
	kv.vecMu.RLock()
	cfg := kv.vecCfg
	ready := kv.vecColumnOK && kv.vecIndexOK
	count := kv.vecCount
	countAt := kv.vecCountAt
	kv.vecMu.RUnlock()

	if !cfg.Enabled || !ready {
		return false
	}

	if time.Since(countAt) > vectorCountTTL {
		if n, err := kv.countEmbeddings(); err != nil {
			log.Printf("keyvalembd: count embeddings: %v", err)
		} else {
			count = n
			kv.vecMu.Lock()
			kv.vecCount = n
			kv.vecCountAt = time.Now()
			kv.vecMu.Unlock()
		}
	}

	return count >= cfg.Threshold
}

// vectorJSON renders a float32 vector as a JSON array, the input format
// accepted by libSQL's vector32() SQL function.
func vectorJSON(v []float32) string {
	var b strings.Builder
	b.Grow(len(v)*10 + 2)
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'g', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// searchByEmbeddingANN performs semantic search through the native libSQL
// vector index. It fetches limit*Oversample candidates from the index and then
// re-ranks them by exact cosine distance, which restores recall to ~100%.
func (kv *KeyValueEmbd) searchByEmbeddingANN(embedding []float32, limit int) ([]SearchResult, error) {
	cfg := kv.VectorIndexConfig()

	candidates := limit * cfg.Oversample
	if candidates < limit {
		candidates = limit
	}

	vjson := vectorJSON(embedding)

	// vector_distance_cos() returns cosine distance (1 - similarity), so the
	// exact re-ranking happens inside SQLite over the ANN candidate set.
	query := fmt.Sprintf(`
		SELECT e.key, e.text,
		       vector_distance_cos(e.%s, vector32(?)) AS dist
		FROM vector_top_k('%s', vector32(?), ?) AS t
		JOIN %s e ON e.rowid = t.id
		ORDER BY dist
		LIMIT ?`,
		vectorColumnName, vectorIndexName, vectorTableName,
	)

	rows, err := kv.db.Query(query, vjson, vjson, candidates, limit)
	if err != nil {
		return nil, fmt.Errorf("vector index query: %w", err)
	}
	defer rows.Close()

	results := make([]SearchResult, 0, limit)
	for rows.Next() {
		var (
			key  string
			text string
			dist float64
		)
		if err := rows.Scan(&key, &text, &dist); err != nil {
			return nil, fmt.Errorf("scan vector index row: %w", err)
		}
		results = append(results, SearchResult{
			Key:   key,
			Text:  text,
			Score: 1 - dist,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vector index rows: %w", err)
	}

	return results, nil
}
