// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kirill-scherba/s3lite"
	"github.com/kirill-scherba/sqlh"
)

// GetInfo retrieves object info (metadata) by key.
func (kv *KeyValueEmbd) GetInfo(key string) (objectInfo *s3lite.ObjectInfo, err error) {
	if !kv.enabled {
		return nil, fmt.Errorf("keyvalembd is not enabled")
	}

	// Use a narrow projection to avoid reading the value BLOB for metadata-only
	// requests.
	type kvDataInfo struct {
		_           bool   `db_table_name:"kv_data"`
		ValueLen    int    `db:"length(value)"`
		ContentType string `db:"content_type"`
		Checksum    string `db:"checksum"`
		CreatedAt   string `db:"created_at"`
		ModifiedAt  string `db:"modified_at"`
		Metadata    string `db:"metadata"`
	}

	row, err := sqlh.Get[kvDataInfo](kv.db, sqlh.Eq("key", key))
	if err == sql.ErrNoRows {
		return nil, s3lite.ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get info for %s: %w", key, err)
	}

	objectInfo = makeObjectInfo(key, row.ValueLen, row.ContentType,
		row.Checksum, row.CreatedAt, row.ModifiedAt, row.Metadata)
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
		b, err := json.Marshal(objectInfo.Metadata)
		if err != nil {
			log.Printf("keyvalembd: SetInfo: marshal metadata: %v", err)
		} else {
			metaJSON = string(b)
		}
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
