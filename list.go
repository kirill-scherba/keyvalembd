// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"iter"
	"log"
	"path/filepath"
	"strings"
)

// List returns an iterator over all keys with the given prefix. Folder
// semantics mimic S3: keys are grouped by directory depth relative to the
// prefix, and sub-folders appear as single entries (trailing slash).
func (kv *KeyValueEmbd) List(prefix string) iter.Seq[string] {
	return func(yield func(key string) bool) {
		if !kv.enabled {
			return
		}

		// Build LIKE pattern: prefix || '%'
		likePattern := prefix + "%"

		rows, err := kv.db.Query(
			"SELECT key FROM kv_data WHERE key LIKE ? ORDER BY key",
			likePattern,
		)
		if err != nil {
			log.Printf("keyvalembd: List: query keys: %v", err)
			return
		}
		defer rows.Close()

		numFoldersInPrefix := strings.Count(prefix, "/")
		if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
			numFoldersInPrefix++
		}

		subfolders := make(map[string]struct{})

		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				log.Printf("keyvalembd: List: scan row: %v", err)
				continue
			}

			// Skip self-folder
			if key == prefix || key == prefix+"/" {
				continue
			}

			// Collapse deeper keys into sub-folder entries
			if strings.Count(key, "/") > numFoldersInPrefix {
				folders := strings.Split(key, "/")
				folderKey := strings.Join(folders[:numFoldersInPrefix+1], "/") + "/"

				if _, ok := subfolders[folderKey]; ok {
					continue
				}
				subfolders[folderKey] = struct{}{}
				key = folderKey
			}

			if !yield(key) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			log.Printf("keyvalembd: List: iterate rows: %v", err)
			return
		}
	}
}

// Count returns the number of keys with the given prefix.
func (kv *KeyValueEmbd) Count(prefix string) (count int) {
	for range kv.List(prefix) {
		count++
	}
	return
}

// IsFolder returns true if key is a folder (ends with '/').
func (kv *KeyValueEmbd) IsFolder(key string) bool {
	l := len(key)
	return l > 0 && key[l-1] == '/'
}

// IsFolderWithFiles returns true if key is a folder and contains files.
func (kv *KeyValueEmbd) IsFolderWithFiles(key string) bool {
	if kv.IsFolder(key) {
		for range kv.List(key) {
			return true
		}
	}
	return false
}

// Dir returns the directory part of the key.
func (kv *KeyValueEmbd) Dir(key string) (dir string) {
	dir = filepath.Dir(key)
	if dir == "." {
		dir = ""
	}
	return
}
