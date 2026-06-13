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
)

// GetInfo retrieves object info (metadata) by key.
func (kv *KeyValueEmbd) GetInfo(key string) (objectInfo *s3lite.ObjectInfo, err error) {
	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}

	var (
		contentType, checksum, createdAt, modifiedAt, metadataStr string
		valLen                                                   int
	)
	err = kv.db.QueryRow(`
		SELECT length(value), content_type, checksum, created_at, modified_at, metadata
		FROM kv_data WHERE key = ?
	`, key).Scan(&valLen, &contentType, &checksum, &createdAt, &modifiedAt, &metadataStr)

	if err == sql.ErrNoRows {
		return nil, s3lite.ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get info for %s: %w", key, err)
	}

	objectInfo = makeObjectInfo(key, valLen, contentType, checksum,
		createdAt, modifiedAt, metadataStr)
	return objectInfo, nil
}

// SetInfo sets object info (metadata) for a key.
func (kv *KeyValueEmbd) SetInfo(key string, objectInfo *s3lite.ObjectInfo) (
	outObjectInfo *s3lite.ObjectInfo, err error) {

	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}

	// Default metadata
	contentType := objectInfo.ContentType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	metaJSON := "{}"
	if objectInfo.Metadata != nil {
		b, _ := json.Marshal(objectInfo.Metadata)
		metaJSON = string(b)
	}
	now := objectInfo.ModifiedAt.UTC().Format(time.RFC3339)
	if objectInfo.ModifiedAt.IsZero() {
		// Use SQLite default for modified_at
		_, err = kv.db.Exec(`
			UPDATE kv_data SET
				content_type = ?,
				metadata = ?
			WHERE key = ?
		`, contentType, metaJSON, key)
	} else {
		_, err = kv.db.Exec(`
			UPDATE kv_data SET
				content_type = ?,
				metadata = ?,
				modified_at = ?
			WHERE key = ?
		`, contentType, metaJSON, now, key)
	}

	if err != nil {
		return nil, fmt.Errorf("set info for %s: %w", key, err)
	}

	// Return current info
	return kv.GetInfo(key)
}
