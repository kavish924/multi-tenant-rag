package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FS stores objects as files under a root directory. It is for local
// development and tests only: Cloud Run's filesystem is not durable.
type FS struct {
	root string
}

// NewFS creates the root directory if needed.
func NewFS(root string) (*FS, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("objectstore: create root: %w", err)
	}
	return &FS{root: abs}, nil
}

func (s *FS) path(key string) (string, error) {
	if _, _, err := ParseDocumentKey(key); err != nil {
		return "", err
	}
	return filepath.Join(s.root, filepath.FromSlash(key)), nil
}

// Put writes to a temporary file and renames it into place, so readers
// never see a partial object and a failed upload leaves nothing behind.
func (s *FS) Put(ctx context.Context, key string, r io.Reader, _ int64, _ string) (err error) {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("objectstore: mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
	if err != nil {
		return fmt.Errorf("objectstore: temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if _, err = io.Copy(tmp, ctxReader{ctx, r}); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("objectstore: close: %w", err)
	}
	if err = os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("objectstore: rename: %w", err)
	}
	return nil
}

// Get opens the object for streaming.
func (s *FS) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := s.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p) //nolint:gosec // path built from a validated document key
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("objectstore: open: %w", err)
	}
	return f, nil
}

// Delete removes the object; a missing object is not an error.
func (s *FS) Delete(_ context.Context, key string) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("objectstore: delete: %w", err)
	}
	return nil
}

// Presign is not available for local files; callers stream instead.
func (s *FS) Presign(context.Context, string, time.Duration) (string, error) {
	return "", ErrPresignNotSupport
}

// ctxReader stops a copy when the context is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// List walks the root and reports every document object (Lister). Temp
// files of in-progress uploads and anything that is not a canonical
// document key are skipped.
func (s *FS) List(ctx context.Context, fn func(ObjectInfo) error) error {
	return filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if _, _, err := ParseDocumentKey(key); err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return fn(ObjectInfo{Key: key, Size: info.Size(), Modified: info.ModTime()})
	})
}
