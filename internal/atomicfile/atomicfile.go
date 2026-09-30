// Package atomicfile writes files so that readers never observe a partially
// written file.
//
// A crash or a restart in the middle of os.WriteFile truncates the target and
// leaves invalid content behind. Writing to a temporary file in the same
// directory and renaming it into place avoids that, because rename is atomic
// within a filesystem.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write stores data at path, replacing any existing file.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Remove the temporary file unless the rename succeeded.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}

	// Flush to stable storage so the data survives a crash before the rename.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("setting permissions on %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}

	return nil
}
