package objectstore

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestFSList(t *testing.T) {
	ctx := context.Background()
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := DocumentKey(uuid.New(), uuid.New()), DocumentKey(uuid.New(), uuid.New())
	for _, k := range []string{a, b} {
		if err := s.Put(ctx, k, strings.NewReader("x"), -1, "text/plain"); err != nil {
			t.Fatal(err)
		}
	}
	// Junk that is not a document key must be ignored.
	junk := filepath.Join(s.root, "tenants", "not-a-uuid", "documents", "x")
	_ = os.MkdirAll(filepath.Dir(junk), 0o750)
	_ = os.WriteFile(junk, []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(filepath.Dir(filepath.Join(s.root, filepath.FromSlash(a))), ".upload-123"), []byte("x"), 0o600)

	got := map[string]bool{}
	if err := s.List(ctx, func(o ObjectInfo) error {
		got[o.Key] = true
		if o.Size != 1 || o.Modified.IsZero() {
			t.Errorf("info %+v", o)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got[a] || !got[b] {
		t.Errorf("listed %v", got)
	}
	var _ Lister = s
}
