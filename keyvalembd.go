// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// KeyValue Embedded — S3-like key-value storage with vector (embedding) search.
//
// This package provides an S3-like key-value store backed by libSQL, with
// optional semantic search via Ollama embeddings. It implements the
// [s3lite.KeyValueStore] interface for drop-in compatibility with code
// that uses S3-style storage.
package keyvalembd

import (
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	// Register libsql driver
	_ "github.com/tursodatabase/go-libsql"

	"github.com/kirill-scherba/s3lite"
)

// KeyValueEmbd is an S3-like key-value store backed by libSQL with optional
// embedding search via Ollama. It implements [s3lite.KeyValueStore].
type KeyValueEmbd struct {
	db      *sql.DB
	dbPath  string
	enabled bool

	embedder *Embedder
}

// New creates a new KeyValueEmbd, opening or creating the libSQL database at
// dbPath. If dbPath is empty, a temporary directory is used. Tables are
// created automatically. Embedder is initialised but may be left in a
// non-ready state if Ollama is unavailable.
func New(dbPath string) (kv *KeyValueEmbd, err error) {
	if dbPath == "" {
		dir, err := os.MkdirTemp("", "keyvalembd-*")
		if err != nil {
			return nil, fmt.Errorf("create temp dir: %w", err)
		}
		dbPath = filepath.Join(dir, "keyvalembd.db")
	}

	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create db directory %s: %w", dir, err)
	}

	// Connect to libSQL with WAL mode and busy timeout
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		dbPath,
	)
	db, err := sql.Open("libsql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err = db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Set pragmas for concurrent access
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			// Non-fatal: pragmas may not be supported by all libSQL builds
		}
	}

	kv = &KeyValueEmbd{
		db:      db,
		dbPath:  dbPath,
		enabled: true,
	}

	// Create tables
	if err = kv.createTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("create tables: %w", err)
	}

	// Initialise embedder (non-fatal if Ollama unavailable)
	kv.embedder = NewEmbedder("", "")

	log.Printf("✅ keyvalembd ready at: %s", dbPath)
	return kv, nil
}

// Close closes the database connection and releases resources.
func (kv *KeyValueEmbd) Close() {
	if kv.db != nil {
		_ = kv.db.Close()
	}
}

// createTables creates the required database tables if they do not exist.
func (kv *KeyValueEmbd) createTables() error {
	// Enable vector extension (best-effort)
	_, _ = kv.db.Exec("CREATE EXTENSION IF NOT EXISTS vector")

	// Main key-value table with metadata
	// NOTE: strftime(...) produces RFC3339; datetime('now') does not.
	_, err := kv.db.Exec(`
		CREATE TABLE IF NOT EXISTS kv_data (
			key          TEXT PRIMARY KEY NOT NULL,
			value        BLOB NOT NULL,
			content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
			checksum     TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			modified_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			metadata     TEXT NOT NULL DEFAULT '{}'
		)
	`)
	if err != nil {
		return fmt.Errorf("create kv_data table: %w", err)
	}

	// Embeddings table
	_, err = kv.db.Exec(`
		CREATE TABLE IF NOT EXISTS kv_embeddings (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			key        TEXT NOT NULL UNIQUE,
			text       TEXT NOT NULL DEFAULT '',
			embedding  BLOB,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
			FOREIGN KEY (key) REFERENCES kv_data(key) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return fmt.Errorf("create kv_embeddings table: %w", err)
	}

	return nil
}

// parseTimestamp tries RFC3339 first, then falls back to SQLite datetime
// format ("2006-01-02 15:04:05"). Returns zero time and logs a warning if
// neither format matches.
func parseTimestamp(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	log.Printf("⚠️  keyvalembd: unparseable timestamp: %q", s)
	return time.Time{}
}

// makeObjectInfo fills an s3lite.ObjectInfo from a database row scan.
func makeObjectInfo(key string, valueLen int, contentType, checksum,
	createdAt, modifiedAt, metadataStr string) *s3lite.ObjectInfo {

	var metadata map[string]string
	_ = json.Unmarshal([]byte(metadataStr), &metadata)

	created := parseTimestamp(createdAt)
	modified := parseTimestamp(modifiedAt)

	info := &s3lite.ObjectInfo{
		ContentLength: int64(valueLen),
		ContentType:   contentType,
		Checksum:      checksum,
		CreatedAt:     created,
		ModifiedAt:    modified,
		Metadata:      metadata,
	}
	if len(key) > 0 && key[len(key)-1] == '/' {
		info.IsFolder = true
		info.ContentType = "application/x-directory"
	}
	return info
}

// computeChecksum returns the MD5 hex checksum of data.
func computeChecksum(data []byte) string {
	return fmt.Sprintf("%x", md5.Sum(data))
}
