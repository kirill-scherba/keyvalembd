// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"math"
	"math/rand"
	"path/filepath"
	"testing"
)

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
		`INSERT OR REPLACE INTO kv_embeddings (key, text, embedding, created_at)
		 VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))`,
		key, key, float32SliceToBytes(vec),
	); err != nil {
		t.Fatalf("insert kv_embeddings: %v", err)
	}
}

// randomVector returns a deterministic pseudo-random unit-ish vector.
func randomVector(rnd *rand.Rand, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(rnd.NormFloat64())
	}
	return v
}

func TestMigrateVectorIndex(t *testing.T) {
	kv := newTestKV(t)

	// No column/index on a fresh database.
	if kv.VectorIndexReady() {
		t.Fatal("vector index reported ready before migration")
	}

	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		insertSynthetic(t, kv, keyName(i), randomVector(rnd, 32))
	}

	if err := kv.MigrateVectorIndex(); err != nil {
		t.Fatalf("MigrateVectorIndex: %v", err)
	}

	if !kv.VectorIndexReady() {
		t.Fatal("vector index not ready after migration")
	}

	// The vector column must be populated for every row.
	var nulls int
	if err := kv.db.QueryRow(
		`SELECT COUNT(*) FROM kv_embeddings WHERE embedding_vec IS NULL`,
	).Scan(&nulls); err != nil {
		t.Fatalf("count null vector column: %v", err)
	}
	if nulls != 0 {
		t.Fatalf("expected 0 null embedding_vec rows, got %d", nulls)
	}

	// Migration must be idempotent.
	if err := kv.MigrateVectorIndex(); err != nil {
		t.Fatalf("second MigrateVectorIndex: %v", err)
	}
}

func TestVectorIndexSearchMatchesScan(t *testing.T) {
	kv := newTestKV(t)

	const (
		dim = 64
		n   = 300
		k   = 10
	)

	rnd := rand.New(rand.NewSource(42))
	vectors := make(map[string][]float32, n)
	for i := 0; i < n; i++ {
		v := randomVector(rnd, dim)
		vectors[keyName(i)] = v
		insertSynthetic(t, kv, keyName(i), v)
	}

	if err := kv.MigrateVectorIndex(); err != nil {
		t.Fatalf("MigrateVectorIndex: %v", err)
	}

	// Force the index path regardless of collection size.
	kv.SetVectorIndexConfig(VectorIndexConfig{Enabled: true, Threshold: 0, Oversample: 5})
	if !kv.VectorIndexReady() {
		t.Fatal("vector index not ready")
	}

	queries := 25
	totalOverlap := 0
	top1Match := 0

	for q := 0; q < queries; q++ {
		query := randomVector(rnd, dim)

		ann, err := kv.searchByEmbeddingANN(query, k)
		if err != nil {
			t.Fatalf("searchByEmbeddingANN: %v", err)
		}
		scan, err := kv.searchByEmbeddingScan(query, k)
		if err != nil {
			t.Fatalf("searchByEmbeddingScan: %v", err)
		}

		if len(ann) == 0 || len(scan) == 0 {
			t.Fatalf("empty results: ann=%d scan=%d", len(ann), len(scan))
		}
		if ann[0].Key == scan[0].Key {
			top1Match++
		}

		scanSet := make(map[string]struct{}, len(scan))
		for _, r := range scan {
			scanSet[r.Key] = struct{}{}
		}
		for _, r := range ann {
			if _, ok := scanSet[r.Key]; ok {
				totalOverlap++
			}
		}
	}

	recallAtK := float64(totalOverlap) / float64(queries*k)
	t.Logf("recall@%d = %.3f, top-1 match = %d/%d", k, recallAtK, top1Match, queries)

	if recallAtK < 0.95 {
		t.Fatalf("recall@%d too low: %.3f", k, recallAtK)
	}
	if top1Match < queries*9/10 {
		t.Fatalf("top-1 match too low: %d/%d", top1Match, queries)
	}
}

func TestVectorIndexThresholdFallsBackToScan(t *testing.T) {
	kv := newTestKV(t)

	rnd := rand.New(rand.NewSource(7))
	for i := 0; i < 10; i++ {
		insertSynthetic(t, kv, keyName(i), randomVector(rnd, 32))
	}
	if err := kv.MigrateVectorIndex(); err != nil {
		t.Fatalf("MigrateVectorIndex: %v", err)
	}

	// Threshold above the collection size: index must not be used.
	kv.SetVectorIndexConfig(VectorIndexConfig{Enabled: true, Threshold: 1000, Oversample: 3})
	if kv.useVectorIndex() {
		t.Fatal("useVectorIndex = true below threshold")
	}

	// Disabled config: index must not be used either.
	kv.SetVectorIndexConfig(VectorIndexConfig{Enabled: false, Threshold: 0, Oversample: 3})
	if kv.useVectorIndex() {
		t.Fatal("useVectorIndex = true when disabled")
	}
}

func TestVectorIndexWriteAndDeletePath(t *testing.T) {
	kv := newTestKV(t)

	const dim = 32
	// Distinctive vector: first component dominates.
	uniq := make([]float32, dim)
	uniq[0] = 1

	// Insert before migration so the column dimension is inferred correctly.
	insertSynthetic(t, kv, "target", uniq)

	if err := kv.MigrateVectorIndex(); err != nil {
		t.Fatalf("MigrateVectorIndex: %v", err)
	}

	kv.SetVectorIndexConfig(VectorIndexConfig{Enabled: true, Threshold: 0, Oversample: 3})

	ann, err := kv.searchByEmbeddingANN(uniq, 1)
	if err != nil {
		t.Fatalf("ANN search: %v", err)
	}
	if len(ann) == 0 || ann[0].Key != "target" {
		t.Fatalf("expected target as top-1, got %+v", ann)
	}

	// Deleting the row must remove it from the index.
	if err := kv.Del("target"); err != nil {
		t.Fatalf("Del: %v", err)
	}
	ann, err = kv.searchByEmbeddingANN(uniq, 1)
	if err != nil {
		t.Fatalf("ANN search after delete: %v", err)
	}
	for _, r := range ann {
		if r.Key == "target" {
			t.Fatalf("deleted key still returned by index: %+v", ann)
		}
	}
}

func TestVectorIndexSearchScore(t *testing.T) {
	kv := newTestKV(t)

	const dim = 16
	a := make([]float32, dim)
	b := make([]float32, dim)
	for i := 0; i < dim; i++ {
		a[i] = 1
		b[i] = 1
	}
	b[0] = -1 // b is not identical to a

	insertSynthetic(t, kv, "a", a)
	insertSynthetic(t, kv, "b", b)
	if err := kv.MigrateVectorIndex(); err != nil {
		t.Fatalf("MigrateVectorIndex: %v", err)
	}
	kv.SetVectorIndexConfig(VectorIndexConfig{Enabled: true, Threshold: 0, Oversample: 3})

	ann, err := kv.searchByEmbeddingANN(a, 2)
	if err != nil {
		t.Fatalf("ANN search: %v", err)
	}
	if len(ann) != 2 {
		t.Fatalf("expected 2 results, got %d", len(ann))
	}
	if ann[0].Key != "a" {
		t.Fatalf("expected a first, got %s", ann[0].Key)
	}
	if math.Abs(ann[0].Score-1.0) > 1e-4 {
		t.Fatalf("expected score ~1.0 for identical vector, got %f", ann[0].Score)
	}
	if ann[1].Score >= ann[0].Score {
		t.Fatalf("scores not ordered: %f >= %f", ann[1].Score, ann[0].Score)
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
