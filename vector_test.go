// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"database/sql"
	"errors"
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

// testDim matches the F32_BLOB dimension declared by KVEmbedding.
const testDim = 768

// newTestKV creates a KeyValueEmbd backed by a temporary database.
func newTestKV(t *testing.T) *KeyValueEmbd {
	t.Helper()
	kv, err := New(filepath.Join(t.TempDir(), "vector-test.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(kv.Close)
	return kv
}

// newTestKVWithIndex creates a KeyValueEmbd with an in-process index enabled.
func newTestKVWithIndex(t *testing.T) *KeyValueEmbd {
	t.Helper()
	kv := newTestKV(t)
	kv.SetVectorIndexDir(filepath.Join(t.TempDir(), "idx"))
	return kv
}

// insertSynthetic inserts a synthetic embedding row directly, bypassing the
// embedder. Used to make tests independent of Ollama.
func insertSynthetic(t *testing.T, kv *KeyValueEmbd, key string, vec []float32) {
	t.Helper()
	if _, err := kv.db.Exec(
		`INSERT OR REPLACE INTO kv_data (key, value) VALUES (?, ?)`,
		key, []byte("v"),
	); err != nil {
		t.Fatalf("insert kv_data: %v", err)
	}
	if _, err := kv.db.Exec(
		`INSERT OR REPLACE INTO kv_embeddings (key, text, embedding_vec, created_at)
		 VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`,
		key, key, float32SliceToBytes(vec),
	); err != nil {
		t.Fatalf("insert kv_embeddings: %v", err)
	}
	kv.markIndexDirty()
}

// randomVector returns a deterministic pseudo-random vector.
func randomVector(rnd *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(rnd.NormFloat64())
	}
	return v
}

func TestVectorIndexSearchMatchesScan(t *testing.T) {
	kv := newTestKVWithIndex(t)

	const n = 300
	rnd := rand.New(rand.NewSource(42))
	for i := 0; i < n; i++ {
		insertSynthetic(t, kv, keyName(i), randomVector(rnd, testDim))
	}

	if !kv.VectorIndexReady() {
		// Not built yet: the first search builds it.
	}

	for q := 0; q < 20; q++ {
		query := randomVector(rnd, testDim)

		indexed, err := kv.searchByEmbeddingIndex(query, 10)
		if err != nil {
			t.Fatalf("searchByEmbeddingIndex: %v", err)
		}
		scanned, err := kv.searchByEmbeddingScan(query, 10)
		if err != nil {
			t.Fatalf("searchByEmbeddingScan: %v", err)
		}
		if len(indexed) != len(scanned) {
			t.Fatalf("query %d: index returned %d hits, scan %d", q, len(indexed), len(scanned))
		}
		for i := range scanned {
			if indexed[i].Key != scanned[i].Key {
				t.Fatalf("query %d rank %d: index %s, scan %s",
					q, i, indexed[i].Key, scanned[i].Key)
			}
			if d := indexed[i].Score - scanned[i].Score; d > 1e-6 || d < -1e-6 {
				t.Fatalf("query %d rank %d: score %f, scan %f",
					q, i, indexed[i].Score, scanned[i].Score)
			}
		}
	}

	if !kv.VectorIndexReady() {
		t.Fatal("index not ready after a search")
	}
}

func TestVectorIndexRebuildsOnWrite(t *testing.T) {
	kv := newTestKVWithIndex(t)

	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 10; i++ {
		insertSynthetic(t, kv, keyName(i), randomVector(rnd, testDim))
	}
	if _, err := kv.searchByEmbeddingIndex(randomVector(rnd, testDim), 5); err != nil {
		t.Fatalf("initial search: %v", err)
	}
	if !kv.VectorIndexReady() {
		t.Fatal("index not ready after the initial search")
	}

	// A distinctive vector: first component dominates.
	uniq := make([]float32, testDim)
	uniq[0] = 1
	insertSynthetic(t, kv, "target", uniq)

	if kv.VectorIndexReady() {
		t.Fatal("index reported ready after a write, want dirty")
	}

	hits, err := kv.searchByEmbeddingIndex(uniq, 1)
	if err != nil {
		t.Fatalf("search after write: %v", err)
	}
	if len(hits) == 0 || hits[0].Key != "target" {
		t.Fatalf("expected target as top-1 after rebuild, got %+v", hits)
	}
}

func TestVectorIndexDeleteRebuilds(t *testing.T) {
	kv := newTestKVWithIndex(t)

	uniq := make([]float32, testDim)
	uniq[0] = 1
	insertSynthetic(t, kv, "target", uniq)

	if _, err := kv.searchByEmbeddingIndex(uniq, 1); err != nil {
		t.Fatalf("search: %v", err)
	}
	if err := kv.Del("target"); err != nil {
		t.Fatalf("Del: %v", err)
	}

	hits, err := kv.searchByEmbeddingIndex(uniq, 1)
	if err != nil {
		t.Fatalf("search after delete: %v", err)
	}
	for _, h := range hits {
		if h.Key == "target" {
			t.Fatalf("deleted key still returned: %+v", hits)
		}
	}
}

// TestVectorIndexReturnsText guards against the index path dropping
// SearchResult.Text: the index stores only key and vector, so the text has to
// be read back from the database.
func TestVectorIndexReturnsText(t *testing.T) {
	kv := newTestKVWithIndex(t)

	vec := make([]float32, testDim)
	vec[0] = 1
	insertSynthetic(t, kv, "with-text", vec)

	hits, err := kv.searchByEmbeddingIndex(vec, 1)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("expected 1 hit, got %d", len(hits))
	}
	// insertSynthetic stores the key as the embedding text.
	if hits[0].Text != "with-text" {
		t.Fatalf("Text = %q, want %q", hits[0].Text, "with-text")
	}
}

func TestVectorIndexDisabled(t *testing.T) {
	kv := newTestKV(t)

	if kv.VectorIndexDir() != "" {
		t.Fatal("index dir should be empty by default")
	}
	if _, err := kv.searchByEmbeddingIndex(make([]float32, testDim), 5); !errors.Is(err, errIndexDisabled) {
		t.Fatalf("searchByEmbeddingIndex = %v, want errIndexDisabled", err)
	}
	if err := kv.RebuildVectorIndex(); !errors.Is(err, errIndexDisabled) {
		t.Fatalf("RebuildVectorIndex = %v, want errIndexDisabled", err)
	}
}

func TestVectorIndexOnEmptyDatabase(t *testing.T) {
	kv := newTestKVWithIndex(t)

	hits, err := kv.searchByEmbeddingIndex(make([]float32, testDim), 5)
	if err != nil {
		t.Fatalf("search on empty database: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected no hits, got %d", len(hits))
	}
}

func TestVectorIndexScore(t *testing.T) {
	kv := newTestKVWithIndex(t)

	a := make([]float32, testDim)
	b := make([]float32, testDim)
	for i := range a {
		a[i] = 1
		b[i] = 1
	}
	b[0] = -1

	insertSynthetic(t, kv, "a", a)
	insertSynthetic(t, kv, "b", b)

	hits, err := kv.searchByEmbeddingIndex(a, 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
	if hits[0].Key != "a" {
		t.Fatalf("expected a first, got %s", hits[0].Key)
	}
	if math.Abs(hits[0].Score-1.0) > 1e-4 {
		t.Fatalf("expected score ~1.0 for identical vector, got %f", hits[0].Score)
	}
	if hits[1].Score >= hits[0].Score {
		t.Fatalf("scores not ordered: %f >= %f", hits[1].Score, hits[0].Score)
	}
}

// newLegacyKV creates a database with the pre-v0.5.0 schema (an embedding BLOB
// column, no embedding_vec) and fills it with synthetic rows.
func newLegacyKV(t *testing.T, n, dim int) *KeyValueEmbd {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE kv_data (
			key TEXT PRIMARY KEY NOT NULL,
			value BLOB NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			checksum TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			modified_at TEXT NOT NULL DEFAULT (datetime('now')),
			metadata TEXT NOT NULL DEFAULT '{}')`,
		`CREATE TABLE kv_embeddings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT NOT NULL UNIQUE,
			text TEXT NOT NULL DEFAULT '',
			embedding BLOB,
			created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}

	rnd := rand.New(rand.NewSource(99))
	for i := 0; i < n; i++ {
		key := keyName(i)
		if _, err := raw.Exec(
			`INSERT INTO kv_data (key, value) VALUES (?, ?)`, key, []byte("v"),
		); err != nil {
			t.Fatalf("legacy kv_data: %v", err)
		}
		if _, err := raw.Exec(
			`INSERT INTO kv_embeddings (key, text, embedding) VALUES (?, ?, ?)`,
			key, key, float32SliceToBytes(randomVector(rnd, dim)),
		); err != nil {
			t.Fatalf("legacy kv_embeddings: %v", err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	kv, err := New(dbPath)
	if err != nil {
		t.Fatalf("New on legacy database: %v", err)
	}
	t.Cleanup(kv.Close)
	return kv
}

// TestLegacyColumnStillWorks covers databases created before v0.5.0: they only
// have the embedding column, which the scan and the index must both use.
func TestLegacyColumnStillWorks(t *testing.T) {
	kv := newLegacyKV(t, 20, testDim)

	if got := kv.vectorColumn(); got != legacyEmbeddingColumn {
		t.Fatalf("vectorColumn() = %q, want %q", got, legacyEmbeddingColumn)
	}

	query := randomVector(rand.New(rand.NewSource(7)), testDim)
	if _, err := kv.SearchByEmbedding(query, 5); err != nil {
		t.Fatalf("scan search on legacy database: %v", err)
	}

	// The in-process index reads from whichever column exists.
	kv.SetVectorIndexDir(filepath.Join(t.TempDir(), "idx"))
	hits, err := kv.searchByEmbeddingIndex(query, 5)
	if err != nil {
		t.Fatalf("indexed search on legacy database: %v", err)
	}
	if len(hits) != 5 {
		t.Fatalf("expected 5 hits, got %d", len(hits))
	}
}

func TestDropLegacyEmbeddingColumnNoop(t *testing.T) {
	kv := newTestKV(t)

	dropped, err := kv.DropLegacyEmbeddingColumn()
	if err != nil {
		t.Fatalf("DropLegacyEmbeddingColumn on fresh database: %v", err)
	}
	if dropped {
		t.Fatal("reported a drop on a database without the legacy column")
	}
}

// keyName returns a stable key name for the i-th synthetic entry.
func keyName(i int) string {
	return "test/key/" + pad(i)
}

// pad renders an integer as a zero-padded 4-digit string.
func pad(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0000"
	}
	var buf [4]byte
	pos := len(buf)
	for i > 0 && pos > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	for pos > 0 {
		pos--
		buf[pos] = '0'
	}
	return string(buf[:])
}
