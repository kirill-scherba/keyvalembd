// Copyright 2026 Kirill Scherba. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !windows

package vecindex

import (
	"os"
	"syscall"
)

// mapFile memory-maps size bytes of f read-only.
func mapFile(f *os.File, size int64) ([]byte, error) {
	return syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
}

// unmapFile releases a mapping created by mapFile.
func unmapFile(b []byte) error {
	if b == nil {
		return nil
	}
	return syscall.Munmap(b)
}
