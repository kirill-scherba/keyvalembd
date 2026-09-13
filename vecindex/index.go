// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package vecindex implements a compact, memory-mapped vector index for
// exact nearest-neighbour search.
//
// On-disk layout (one directory):
//
//	meta.json    index metadata: version, dim, count, built_at
//	vectors.bin  count × dim float32, little-endian
//	keys.bin     (count+1) × uint64 offsets, followed by concatenated key bytes
//
// Both data files are memory-mapped read-only. The kernel pages in what is
// actually touched and can reclaim those pages under memory pressure, so the
// process never holds the whole collection on its Go heap. That is the point
// of this package: no per-query reads from the database, and no unbounded
// heap growth.
//
// At roughly dim*4 bytes per vector plus a small key table, the index is about
// 3 KB per 768-dim vector — the same order as pgvector's HNSW, and about 50x
// smaller than libSQL's DiskANN index.
//
// Search is an exact cosine scan over the mapped vectors: 100% recall, no
// approximation. A graph layer for sublinear search can be added on top of the
// same file format later without changing this API.
package vecindex

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

const (
	metaName    = "meta.json"
	vectorsName = "vectors.bin"
	keysName    = "keys.bin"
	formatVer   = 1
)

// meta is the contents of meta.json.
type meta struct {
	Version int    `json:"version"`
	Dim     int    `json:"dim"`
	Count   int    `json:"count"`
	BuiltAt string `json:"built_at"`
}

// Index is a memory-mapped vector index.
type Index struct {
	dir string
	m   meta

	vecFile *os.File
	keyFile *os.File
	vecMap  []byte
	keyMap  []byte

	vecs   []float32 // view into vecMap
	keyOff []uint64  // view into keyMap
	keyBlob []byte   // view into keyMap
}

// Hit is a single search result.
type Hit struct {
	Key   string
	Score float64
}

// EntrySource yields the next entry to index. It returns ok=false when the
// source is exhausted.
type EntrySource func() (key string, vec []float32, ok bool, err error)

// Build writes a new index into dir, streaming entries from next. The
// directory is created if needed. meta.json is written last, so a partially
// written index is detectable and will fail to Open.
func Build(dir string, dim int, next EntrySource) (*Index, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("vecindex: dim must be positive, got %d", dim)
	}
	if next == nil {
		return nil, fmt.Errorf("vecindex: nil entry source")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("vecindex: create dir: %w", err)
	}

	vecPath := filepath.Join(dir, vectorsName)
	keyPath := filepath.Join(dir, keysName)

	vf, err := os.Create(vecPath)
	if err != nil {
		return nil, fmt.Errorf("vecindex: create vectors: %w", err)
	}
	defer vf.Close()

	kf, err := os.Create(keyPath)
	if err != nil {
		return nil, fmt.Errorf("vecindex: create keys: %w", err)
	}
	defer kf.Close()

	// Reserve the offset table, then append key bytes after it. We cannot know
	// the count in advance, so offsets are written first and the key blob is
	// written in a second pass appended at a known position.
	offsets := make([]uint64, 0, 4096)
	keyBlob := make([]byte, 0, 4096)
	var count int

	buf := make([]byte, dim*4)
	for {
		key, vec, ok, err := next()
		if err != nil {
			return nil, fmt.Errorf("vecindex: read entry %d: %w", count, err)
		}
		if !ok {
			break
		}
		if len(vec) != dim {
			return nil, fmt.Errorf("vecindex: entry %q has dim %d, want %d",
				key, len(vec), dim)
		}

		for i, f := range vec {
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
		}
		if _, err := vf.Write(buf); err != nil {
			return nil, fmt.Errorf("vecindex: write vector %d: %w", count, err)
		}

		offsets = append(offsets, uint64(len(keyBlob)))
		keyBlob = append(keyBlob, key...)
		count++
	}
	offsets = append(offsets, uint64(len(keyBlob)))

	// Offsets, then the key blob.
	offBytes := make([]byte, len(offsets)*8)
	for i, o := range offsets {
		binary.LittleEndian.PutUint64(offBytes[i*8:], o)
	}
	if _, err := kf.Write(offBytes); err != nil {
		return nil, fmt.Errorf("vecindex: write key offsets: %w", err)
	}
	if _, err := kf.Write(keyBlob); err != nil {
		return nil, fmt.Errorf("vecindex: write key blob: %w", err)
	}

	if err := vf.Sync(); err != nil {
		return nil, fmt.Errorf("vecindex: sync vectors: %w", err)
	}
	if err := kf.Sync(); err != nil {
		return nil, fmt.Errorf("vecindex: sync keys: %w", err)
	}

	m := meta{
		Version: formatVer,
		Dim:     dim,
		Count:   count,
		BuiltAt: nowRFC3339(),
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("vecindex: marshal meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, metaName), mb, 0o644); err != nil {
		return nil, fmt.Errorf("vecindex: write meta: %w", err)
	}

	return Open(dir)
}

// Open memory-maps an index previously written by Build.
func Open(dir string) (*Index, error) {
	mb, err := os.ReadFile(filepath.Join(dir, metaName))
	if err != nil {
		return nil, fmt.Errorf("vecindex: read meta: %w", err)
	}
	var m meta
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("vecindex: parse meta: %w", err)
	}
	if m.Version != formatVer {
		return nil, fmt.Errorf("vecindex: unsupported format version %d", m.Version)
	}
	if m.Dim <= 0 || m.Count < 0 {
		return nil, fmt.Errorf("vecindex: corrupt meta (dim=%d count=%d)", m.Dim, m.Count)
	}

	ix := &Index{dir: dir, m: m}

	vecSize := int64(m.Count) * int64(m.Dim) * 4
	if ix.vecMap, ix.vecFile, err = mmapFile(filepath.Join(dir, vectorsName), vecSize); err != nil {
		return nil, err
	}
	keySize := int64(m.Count+1)*8 + int64(keyBlobSize(ix, dir))
	if ix.keyMap, ix.keyFile, err = mmapFile(filepath.Join(dir, keysName), keySize); err != nil {
		ix.Close()
		return nil, err
	}

	ix.vecs = bytesToFloat32(ix.vecMap)
	ix.keyOff = bytesToUint64(ix.keyMap[:int(m.Count+1)*8])
	ix.keyBlob = ix.keyMap[int(m.Count+1)*8:]

	return ix, nil
}

// keyBlobSize returns the size of the key blob section (file size minus the
// offset table).
func keyBlobSize(ix *Index, dir string) int {
	fi, err := os.Stat(filepath.Join(dir, keysName))
	if err != nil {
		return 0
	}
	return int(fi.Size()) - (ix.m.Count+1)*8
}

// mmapFile opens path and maps size bytes read-only. A zero size yields a nil
// mapping, which is valid for an empty index.
func mmapFile(path string, size int64) ([]byte, *os.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("vecindex: open %s: %w", filepath.Base(path), err)
	}
	if size <= 0 {
		return nil, f, nil
	}
	b, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("vecindex: mmap %s: %w", filepath.Base(path), err)
	}
	return b, f, nil
}

// Count returns the number of indexed vectors.
func (ix *Index) Count() int { return ix.m.Count }

// Dim returns the vector dimension.
func (ix *Index) Dim() int { return ix.m.Dim }

// DiskBytes returns the size of the two data files in bytes.
func (ix *Index) DiskBytes() int64 {
	return int64(len(ix.vecMap)) + int64(len(ix.keyMap))
}

// Search returns the k vectors most similar to query, by exact cosine
// similarity. Results are ordered by descending score.
func (ix *Index) Search(query []float32, k int) []Hit {
	if k <= 0 || ix.m.Count == 0 {
		return nil
	}
	if len(query) != ix.m.Dim {
		return nil
	}
	if k > ix.m.Count {
		k = ix.m.Count
	}

	h := &topKHeap{}
	dim := ix.m.Dim
	for i := 0; i < ix.m.Count; i++ {
		score := cosine(query, ix.vecs[i*dim:(i+1)*dim])
		switch {
		case h.Len() < k:
			heapPush(h, candidate{idx: i, score: score})
		case score > (*h)[0].score:
			(*h)[0] = candidate{idx: i, score: score}
			heapFix(h, 0)
		}
	}

	out := make([]Hit, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		c := heapPop(h)
		out[i] = Hit{Key: ix.Key(c.idx), Score: c.score}
	}
	return out
}

// Key returns the key of the i-th vector.
func (ix *Index) Key(i int) string {
	if i < 0 || i >= ix.m.Count {
		return ""
	}
	lo := ix.keyOff[i]
	hi := ix.keyOff[i+1]
	if lo > hi || hi > uint64(len(ix.keyBlob)) {
		return ""
	}
	return string(ix.keyBlob[lo:hi])
}

// Keys returns all keys, in index order.
func (ix *Index) Keys() []string {
	out := make([]string, ix.m.Count)
	for i := range out {
		out[i] = ix.Key(i)
	}
	return out
}

// Close unmaps the files and releases the descriptors.
func (ix *Index) Close() error {
	var firstErr error
	if ix.vecMap != nil {
		if err := syscall.Munmap(ix.vecMap); err != nil {
			firstErr = err
		}
		ix.vecMap = nil
	}
	if ix.keyMap != nil {
		if err := syscall.Munmap(ix.keyMap); err != nil && firstErr == nil {
			firstErr = err
		}
		ix.keyMap = nil
	}
	if ix.vecFile != nil {
		ix.vecFile.Close()
		ix.vecFile = nil
	}
	if ix.keyFile != nil {
		ix.keyFile.Close()
		ix.keyFile = nil
	}
	return firstErr
}

// ─── helpers ────────────────────────────────────────────────────────────────

// nowRFC3339 returns the current UTC time in RFC3339 form.
func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// bytesToFloat32 reinterprets a byte slice as float32 without copying. The
// slice must be 4-byte aligned (mmap is page aligned).
func bytesToFloat32(b []byte) []float32 {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), len(b)/4)
}

// bytesToUint64 reinterprets a byte slice as uint64 without copying.
func bytesToUint64(b []byte) []uint64 {
	if len(b) == 0 {
		return nil
	}
	return unsafe.Slice((*uint64)(unsafe.Pointer(&b[0])), len(b)/8)
}

// cosine computes the cosine similarity between two float32 vectors.
func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// topKHeap is a min-heap of candidates, used to keep the k best scores while
// scanning without allocating per candidate.
type candidate struct {
	idx   int
	score float64
}

type topKHeap []candidate

func (h topKHeap) Len() int            { return len(h) }
func (h topKHeap) Less(i, j int) bool  { return h[i].score < h[j].score }
func (h topKHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *topKHeap) Push(x any)         { *h = append(*h, x.(candidate)) }
func (h *topKHeap) Pop() any           { old := *h; n := len(old); x := old[n-1]; *h = old[:n-1]; return x }

// Small wrappers so the search loop does not pull in container/heap's
// interface calls at every step.
func heapPush(h *topKHeap, c candidate) { h.Push(c); siftUp(h, h.Len()-1) }
func heapPop(h *topKHeap) candidate     { n := h.Len() - 1; h.Swap(0, n); siftDown(h, 0, n); return h.Pop().(candidate) }
func heapFix(h *topKHeap, i int)        { siftDown(h, i, h.Len()) }

func siftUp(h *topKHeap, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !h.Less(i, parent) {
			break
		}
		h.Swap(i, parent)
		i = parent
	}
}

func siftDown(h *topKHeap, i, n int) {
	for {
		l := 2*i + 1
		if l >= n {
			return
		}
		small := l
		if r := l + 1; r < n && h.Less(r, l) {
			small = r
		}
		if !h.Less(small, i) {
			return
		}
		h.Swap(i, small)
		i = small
	}
}
