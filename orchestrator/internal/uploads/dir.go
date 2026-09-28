// Package uploads stores the bytes of presigned document uploads.
//
// In local development and the Sepolia simulation that is a directory. Production would be object storage
// behind the same one-method interface; nothing above this package knows which.
package uploads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Dir keeps each upload as one file named by its document id.
type Dir struct{ root string }

// NewDir creates the directory if needed.
func NewDir(root string) (*Dir, error) {
	if root == "" {
		return nil, errors.New("uploads: no directory")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("uploads: creating %s: %w", root, err)
	}
	return &Dir{root: root}, nil
}

// idRe is the only name a stored object may have. The document id is a UUID the database generated, so
// nothing a caller supplied can reach the filesystem path.
var idRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Put writes the bytes atomically: a reader never sees half a file, and a retry replaces the whole of it.
func (d *Dir) Put(ctx context.Context, id string, content []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !idRe.MatchString(id) {
		return fmt.Errorf("uploads: %q is not a document id", id)
	}
	tmp, err := os.CreateTemp(d.root, id+".*.part")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(d.root, id))
}

// Get reads an upload back.
func (d *Dir) Get(id string) ([]byte, error) {
	if !idRe.MatchString(id) {
		return nil, fmt.Errorf("uploads: %q is not a document id", id)
	}
	return os.ReadFile(filepath.Join(d.root, id))
}
