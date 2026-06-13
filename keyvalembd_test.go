// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kirill-scherba/s3lite"
)

func TestKeyValueEmbd(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "keyvalembd-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := tmpDir + "/test.db"

	var kv *KeyValueEmbd
	defer func() {
		if kv != nil {
			kv.Close()
		}
	}()

	type keyValue struct {
		key   string
		value []byte
	}

	t.Run("New", func(t *testing.T) {
		var err error
		kv, err = New(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Log("keyvalembd created at", kv.dbPath)
	})

	t.Run("Model", func(t *testing.T) {
		model := kv.embedder.Model()
		require(t, "embeddinggemma:latest", model)
		t.Log("embedding model:", model)
	})

	t.Run("Set and Get", func(t *testing.T) {
		keys := []keyValue{
			{"key1", []byte("value1")},
			{"key2", []byte("value2")},
			{"key3", []byte("value3")},
		}

		for _, k := range keys {
			_, err := kv.Set(k.key, k.value)
			if err != nil {
				t.Fatal(err)
			}
			t.Log("set", k.key)
		}

		for _, k := range keys {
			data, err := kv.Get(k.key)
			if err != nil {
				t.Fatal(err)
			}
			require(t, string(k.value), string(data))
			t.Log("get", k.key, string(data))
		}
	})

	t.Run("Get not found", func(t *testing.T) {
		_, err := kv.Get("nonexistent")
		if err != s3lite.ErrKeyNotFound {
			t.Fatal("expected ErrKeyNotFound, got", err)
		}
	})

	t.Run("Set with ObjectInfo", func(t *testing.T) {
		info, err := kv.Set("info-key", []byte("info-value"),
			s3lite.SetContentType("text/plain"))
		if err != nil {
			t.Fatal(err)
		}
		if info.ContentType != "text/plain" {
			t.Fatal("expected text/plain, got", info.ContentType)
		}
		t.Log("Set with ObjectInfo:", info.ContentType, info.Checksum)
	})

	t.Run("GetInfo", func(t *testing.T) {
		info, err := kv.GetInfo("info-key")
		if err != nil {
			t.Fatal(err)
		}
		if info.ContentType != "text/plain" {
			t.Fatal("expected text/plain, got", info.ContentType)
		}
		if info.ContentLength != 10 {
			t.Fatal("expected content length 10, got", info.ContentLength)
		}
		if info.Checksum == "" {
			t.Fatal("expected non-empty checksum")
		}
		t.Logf("GetInfo: type=%s, len=%d, checksum=%s",
			info.ContentType, info.ContentLength, info.Checksum)
	})

	t.Run("SetInfo", func(t *testing.T) {
		newMeta := map[string]string{"author": "test", "version": "1"}
		outInfo, err := kv.SetInfo("info-key", &s3lite.ObjectInfo{
			ContentType: "text/markdown",
			Metadata:    newMeta,
		})
		if err != nil {
			t.Fatal(err)
		}
		if outInfo.ContentType != "text/markdown" {
			t.Fatal("expected text/markdown, got", outInfo.ContentType)
		}
		if outInfo.Metadata["author"] != "test" {
			t.Fatal("expected metadata author=test, got", outInfo.Metadata["author"])
		}
		t.Logf("SetInfo: type=%s, meta=%v", outInfo.ContentType, outInfo.Metadata)
	})

	t.Run("Del", func(t *testing.T) {
		err := kv.Del("key1")
		if err != nil {
			t.Fatal(err)
		}
		_, err = kv.Get("key1")
		if err != s3lite.ErrKeyNotFound {
			t.Fatal("expected ErrKeyNotFound after delete, got", err)
		}
		t.Log("deleted key1")
	})

	t.Run("List flat", func(t *testing.T) {
		t.Log("List all keys:")
		for key := range kv.List("") {
			t.Log(" -", key)
		}
	})

	t.Run("List folders", func(t *testing.T) {
		folder := "test/"
		subfolder := folder + "subfolder/"

		keys := []keyValue{
			{folder + "key1", []byte("value1")},
			{folder + "key2", []byte("value2")},
			{folder + "key3", []byte("value3")},
		}
		keys2 := []keyValue{
			{subfolder + "key1", []byte("value1")},
			{subfolder + "key2", []byte("value2")},
			{subfolder + "key3", []byte("value3")},
		}
		extraKeys := []keyValue{
			{"new key4", []byte("value4")},
			{"a new key5", []byte("value5")},
			{"afolder/key6", []byte("value6")},
			{"afolder/key7", []byte("value7")},
		}

		all := append(append(keys, keys2...), extraKeys...)
		for _, k := range all {
			_, err := kv.Set(k.key, k.value)
			if err != nil {
				t.Fatal(err)
			}
		}

		t.Log("List root:")
		for key := range kv.List("") {
			info, _ := kv.GetInfo(key)
			if info != nil {
				t.Logf("  '%s' %d bytes %s", key, info.ContentLength, info.Checksum)
			} else {
				t.Logf("  '%s'", key)
			}
		}

		t.Log("List folder:", folder)
		for key := range kv.List(folder) {
			t.Log("  ", key)
		}

		t.Log("List subfolder:", subfolder)
		for key := range kv.List(subfolder) {
			t.Log("  ", key)
		}
	})

	t.Run("Count", func(t *testing.T) {
		count := kv.Count("")
		t.Log("Total keys:", count)
		// After deleting key1, we have: info-key, key2, key3,
		// plus from List folders: a new key5, afolder/, new key4, test/
		// Total: 7
		if count != 7 {
			t.Fatalf("expected 7 keys, got %d", count)
		}

		folderCount := kv.Count("test/")
		t.Log("Keys in test/:", folderCount)
		// test/ has: test/key1, test/key2, test/key3, test/subfolder/
		// Total: 4
		if folderCount != 4 {
			t.Fatal("expected 4 entries in test/, got", folderCount)
		}
	})

	t.Run("IsFolder", func(t *testing.T) {
		if !kv.IsFolder("test/") {
			t.Fatal("expected test/ to be a folder")
		}
		if kv.IsFolder("test") {
			t.Fatal("expected test not to be a folder")
		}
		if kv.IsFolder("") {
			t.Fatal("expected empty string not to be a folder")
		}
	})

	t.Run("IsFolderWithFiles", func(t *testing.T) {
		if !kv.IsFolderWithFiles("test/") {
			t.Fatal("expected test/ to have files")
		}
		if kv.IsFolderWithFiles("nonexistent/") {
			t.Fatal("expected nonexistent/ to not have files")
		}
	})

	t.Run("Dir", func(t *testing.T) {
		require(t, "test", kv.Dir("test/key1"))
		require(t, "test/folder", kv.Dir("test/folder/key1"))
		require(t, "", kv.Dir("key1"))
	})

	t.Run("SetWithEmbedding", func(t *testing.T) {
		info, err := kv.SetWithEmbedding("emb-key", []byte("emb-value"),
			"some text for embedding")
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("SetWithEmbedding: %s, embedding ready=%v",
			info.Checksum, kv.embedder.Ready())

		if kv.embedder.Ready() {
			var count int
			err := kv.db.QueryRow(
				"SELECT COUNT(*) FROM kv_embeddings WHERE key = ?",
				"emb-key").Scan(&count)
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatal("expected 1 embedding record, got", count)
			}
		}
	})

	t.Run("SearchSemantic (if available)", func(t *testing.T) {
		if kv.embedder == nil || !kv.embedder.Ready() {
			t.Skip("embedder not ready, skipping semantic search test")
		}

		results, err := kv.SearchSemantic("some text", 5)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("SearchSemantic results: %+v", results)
	})

	t.Run("Sequential operations", func(t *testing.T) {
		const numKeys = 50
		start := time.Now()

		for i := range numKeys {
			key := fmt.Sprintf("seq-key-%d", i)
			_, err := kv.Set(key, []byte(key))
			if err != nil {
				t.Fatal(err)
			}
		}

		for i := range numKeys {
			key := fmt.Sprintf("seq-key-%d", i)
			data, err := kv.Get(key)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != key {
				t.Fatalf("expected %s, got %s", key, string(data))
			}
		}

		elapsed := time.Since(start)
		opsPerSec := float64(numKeys*2) / elapsed.Seconds()
		t.Logf("Sequential: %d keys, %.2f ops/s", numKeys, opsPerSec)
	})

	t.Run("Close", func(t *testing.T) {
		kv.Close()
		kv = nil
	})
}

func require[T comparable](t *testing.T, expected, actual T) {
	t.Helper()
	if expected != actual {
		t.Fatalf("expected %v, got %v", expected, actual)
	}
}
