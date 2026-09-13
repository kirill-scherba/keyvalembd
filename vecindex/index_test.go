// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vecindex

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"
)

// makeSource returns an EntrySource over n deterministic vectors.
func makeSource(rnd *rand.Rand, n, dim int) (EntrySource, [][]float32) {
	vecs := make([][]float32, n)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(rnd.NormFloat64())
		}
		vecs[i] = v
	}
	i := 0
	return func() (string, []float32, bool, error) {
		if i >= n {
			return "", nil, false, nil
		}
		key := fmt.Sprintf("key/%05d", i)
		v := vecs[i]
		i++
		return key, v, true, nil
	}, vecs
}

// referenceTopK is an independent exact implementation used as ground truth.
func referenceTopK(vecs [][]float32, q []float32, k int) []Hit {
	hits := make([]Hit, 0, len(vecs))
	for i, v := range vecs {
		hits = append(hits, Hit{Key: fmt.Sprintf("key/%05d", i), Score: cosine(q, v)})
	}
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j].Score > hits[j-1].Score; j-- {
			hits[j], hits[j-1] = hits[j-1], hits[j]
		}
	}
	if k > len(hits) {
		k = len(hits)
	}
	return hits[:k]
}

func TestBuildOpenSearch(t *testing.T) {
	const (
		n   = 500
		dim = 64
		k   = 10
	)
	dir := filepath.Join(t.TempDir(), "idx")
	rnd := rand.New(rand.NewSource(1))

	src, vecs := makeSource(rnd, n, dim)
	ix, err := Build(dir, dim, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ix.Close()

	if ix.Count() != n {
		t.Fatalf("Count() = %d, want %d", ix.Count(), n)
	}
	if ix.Dim() != dim {
		t.Fatalf("Dim() = %d, want %d", ix.Dim(), dim)
	}

	// Search must match the independent reference exactly.
	for q := 0; q < 20; q++ {
		query := vecs[rnd.Intn(n)]
		got := ix.Search(query, k)
		want := referenceTopK(vecs, query, k)
		if len(got) != len(want) {
			t.Fatalf("query %d: got %d hits, want %d", q, len(got), len(want))
		}
		for i := range want {
			if got[i].Key != want[i].Key {
				t.Fatalf("query %d rank %d: got %s, want %s", q, i, got[i].Key, want[i].Key)
			}
			if diff := got[i].Score - want[i].Score; diff > 1e-6 || diff < -1e-6 {
				t.Fatalf("query %d rank %d: score %f, want %f", q, i, got[i].Score, want[i].Score)
			}
		}
	}
}

func TestReopen(t *testing.T) {
	const (
		n   = 100
		dim = 32
	)
	dir := filepath.Join(t.TempDir(), "idx")
	rnd := rand.New(rand.NewSource(2))
	src, _ := makeSource(rnd, n, dim)

	ix, err := Build(dir, dim, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ix.Close()

	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer reopened.Close()

	if reopened.Count() != n {
		t.Fatalf("Count() = %d, want %d", reopened.Count(), n)
	}
	if got := reopened.Key(0); got != "key/00000" {
		t.Fatalf("Key(0) = %q", got)
	}
	if got := reopened.Key(n - 1); got != fmt.Sprintf("key/%05d", n-1) {
		t.Fatalf("Key(%d) = %q", n-1, got)
	}
	q := make([]float32, dim)
	for i := range q {
		q[i] = 1
	}
	if hits := reopened.Search(q, 5); len(hits) != 5 {
		t.Fatalf("Search returned %d hits, want 5", len(hits))
	}
}

func TestEmptyIndex(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "empty")
	src, _ := makeSource(rand.New(rand.NewSource(3)), 0, 16)

	ix, err := Build(dir, 16, src)
	if err != nil {
		t.Fatalf("Build empty: %v", err)
	}
	defer ix.Close()

	if ix.Count() != 0 {
		t.Fatalf("Count() = %d, want 0", ix.Count())
	}
	if hits := ix.Search(make([]float32, 16), 5); hits != nil {
		t.Fatalf("Search on empty index = %v, want nil", hits)
	}
}

func TestDimensionMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "idx")
	src, _ := makeSource(rand.New(rand.NewSource(4)), 3, 8)
	ix, err := Build(dir, 8, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ix.Close()

	if hits := ix.Search(make([]float32, 7), 3); hits != nil {
		t.Fatalf("Search with wrong dim returned %v, want nil", hits)
	}

	// A source that yields a wrong dimension must fail the build.
	bad := func() (string, []float32, bool, error) {
		return "k", make([]float32, 5), true, nil
	}
	if _, err := Build(filepath.Join(t.TempDir(), "bad"), 8, bad); err == nil {
		t.Fatal("Build accepted a vector with the wrong dimension")
	}
}

func TestDiskSizeIsCompact(t *testing.T) {
	const (
		n   = 1000
		dim = 768
	)
	dir := filepath.Join(t.TempDir(), "idx")
	src, _ := makeSource(rand.New(rand.NewSource(5)), n, dim)
	ix, err := Build(dir, dim, src)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer ix.Close()

	raw := int64(n) * dim * 4
	if ix.DiskBytes() < raw {
		t.Fatalf("DiskBytes() = %d, smaller than the raw vectors %d", ix.DiskBytes(), raw)
	}
	// Vectors plus a small key table; well under 4 KB per vector.
	perVector := ix.DiskBytes() / n
	if perVector > 4096 {
		t.Fatalf("DiskBytes() per vector = %d bytes, want under 4096", perVector)
	}
	t.Logf("disk: %d bytes for %d vectors (%d bytes/vector, raw=%d)",
		ix.DiskBytes(), n, perVector, raw)
}

func BenchmarkSearch(b *testing.B) {
	const (
		n   = 6847
		dim = 768
		k   = 10
	)
	dir := filepath.Join(b.TempDir(), "idx")
	rnd := rand.New(rand.NewSource(6))
	src, vecs := makeSource(rnd, n, dim)

	ix, err := Build(dir, dim, src)
	if err != nil {
		b.Fatalf("Build: %v", err)
	}
	defer ix.Close()

	query := vecs[0]
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.Search(query, k)
	}
}
