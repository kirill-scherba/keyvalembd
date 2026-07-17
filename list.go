// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package keyvalembd

import (
	"context"
	"iter"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/kirill-scherba/sqlh"
)

// kvDataKey is a lightweight projection of KVData used by List to avoid
// loading the BLOB value column when only keys are needed.
type kvDataKey struct {
	_   bool   `db_table_name:"kv_data"`
	Key string `db:"key"`
}

// ListOption configures optional filtering in List.
type ListOption func(*listOptions)

type listOptions struct {
	dateFrom string
	dateTo   string
}

// ListWithDateFrom filters keys created at or after the given time.
func ListWithDateFrom(t time.Time) ListOption {
	return func(o *listOptions) {
		o.dateFrom = t.UTC().Format("2006-01-02T15:04:05Z")
	}
}

// ListWithDateTo filters keys created at or before the given time.
func ListWithDateTo(t time.Time) ListOption {
	return func(o *listOptions) {
		o.dateTo = t.UTC().Format("2006-01-02T15:04:05Z")
	}
}

// List returns an iterator over all keys with the given prefix. Folder
// semantics mimic S3: keys are grouped by directory depth relative to the
// prefix, and sub-folders appear as single entries (trailing slash).
// Optional ListOption arguments filter by created_at range.
func (kv *KeyValueEmbd) List(prefix string, opts ...ListOption) iter.Seq[string] {
	return func(yield func(key string) bool) {
		if !kv.enabled {
			return
		}

		likePattern := prefix + "%"

		numFoldersInPrefix := strings.Count(prefix, "/")
		if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
			numFoldersInPrefix++
		}

		// Apply options
		var o listOptions
		for _, opt := range opts {
			opt(&o)
		}

		// Build conditions for sqlh.ListRange.
		// Order: filter attrs first, then error func, then context.
		conds := []any{sqlh.Like("key", likePattern)}
		if o.dateFrom != "" {
			conds = append(conds, sqlh.Gte("created_at", o.dateFrom))
		}
		if o.dateTo != "" {
			conds = append(conds, sqlh.Lte("created_at", o.dateTo))
		}
		conds = append(conds,
			func(err error) { log.Printf("keyvalembd: List: iterate: %v", err) },
			context.Background(),
		)

		subfolders := make(map[string]struct{})

		for _, row := range sqlh.ListRange[kvDataKey](
			kv.db, 0, "", "key ASC", 0,
			conds...,
		) {
			key := row.Key

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
