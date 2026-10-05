// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build windows

package vecindex

import (
	"io"
	"os"
)

// mapFile reads size bytes of f into memory. Windows has no syscall.Mmap in the
// standard library, so the index is served from an in-memory copy.
func mapFile(f *os.File, size int64) ([]byte, error) {
	b := make([]byte, size)
	if _, err := f.ReadAt(b, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return b, nil
}

// unmapFile is a no-op for the in-memory fallback.
func unmapFile(b []byte) error { return nil }
