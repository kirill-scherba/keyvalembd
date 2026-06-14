// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kirill-scherba/s3lite"
	"github.com/kirill-scherba/sqlh"
)

// Get retrieves a value by its key from the database.
func (kv *KeyValueEmbd) Get(key string) (value []byte, err error) {
	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}
	row, err := sqlh.Get[KVData](kv.db, sqlh.Eq("key", key))
	if err == sql.ErrNoRows {
		return nil, s3lite.ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get key %s: %w", key, err)
	}
	return row.Value, nil
}

// Set sets a key-value pair. Optionally accepts ObjectInfo for setting
// content type and metadata.
func (kv *KeyValueEmbd) Set(key string, value []byte, info ...*s3lite.ObjectInfo) (
	objectInfo *s3lite.ObjectInfo, err error) {

	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}

	// Build metadata from optional ObjectInfo
	var contentType = "application/octet-stream"
	var metaMap = make(map[string]string)
	if len(info) > 0 && info[0] != nil {
		if info[0].ContentType != "" {
			contentType = info[0].ContentType
		}
		if info[0].Metadata != nil {
			metaMap = info[0].Metadata
		}
	}

	checksum := computeChecksum(value)
	metaJSON, _ := json.Marshal(metaMap)
	now := time.Now().UTC().Format(time.RFC3339)

	// Upsert: insert or update
	_, err = kv.db.Exec(`
		INSERT INTO kv_data (key, value, content_type, checksum, created_at, modified_at, metadata)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value = excluded.value,
			content_type = excluded.content_type,
			checksum = excluded.checksum,
			modified_at = excluded.modified_at,
			metadata = excluded.metadata
	`, key, value, contentType, checksum, now, now, string(metaJSON))
	if err != nil {
		return nil, fmt.Errorf("set key %s: %w", key, err)
	}

	// Build return ObjectInfo
	objectInfo = &s3lite.ObjectInfo{
		ContentLength: int64(len(value)),
		ContentType:   contentType,
		Checksum:      checksum,
		CreatedAt:     time.Now(),
		ModifiedAt:    time.Now(),
		Metadata:      metaMap,
	}
	if kv.IsFolder(key) {
		objectInfo.IsFolder = true
		objectInfo.ContentType = "application/x-directory"
	}
	return objectInfo, nil
}

// SetWithEmbedding stores a value and generates an embedding from the given
// text, saving both to the database.
func (kv *KeyValueEmbd) SetWithEmbedding(key string, value []byte,
	text string, info ...*s3lite.ObjectInfo) (*s3lite.ObjectInfo, error) {

	// Set the value first (uses standard Set)
	objectInfo, err := kv.Set(key, value, info...)
	if err != nil {
		return nil, err
	}

	// Generate embedding if embedder is ready
	if kv.embedder != nil && kv.embedder.Ready() {
		emb, err := kv.embedder.GenerateEmbedding(text)
		if err != nil {
			// Non-fatal: embedding generation failure doesn't affect value storage
			return objectInfo, nil
		}

		embBytes := float32SliceToBytes(emb)
		_, err = kv.db.Exec(`
			INSERT INTO kv_embeddings (key, text, embedding, created_at)
			VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
			ON CONFLICT(key) DO UPDATE SET
				text = excluded.text,
				embedding = excluded.embedding
		`, key, text, embBytes)
		if err != nil {
			// Non-fatal
			return objectInfo, nil
		}
	}

	return objectInfo, nil
}

// Del deletes one or more keys from the database (cascades to embeddings).
func (kv *KeyValueEmbd) Del(keys ...string) (err error) {
	if !kv.enabled {
		return fmt.Errorf("keyvalembd is not enabled")
	}

	for _, key := range keys {
		if err := sqlh.Delete[KVData](kv.db, sqlh.Eq("key", key)); err != nil {
			return fmt.Errorf("delete key %s: %w", key, err)
		}
	}
	return nil
}
