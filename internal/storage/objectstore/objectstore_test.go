package objectstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/uuid"
)

// STO-AC-004: one canonical key shape.
func TestDocumentKey(t *testing.T) {
	tid, did := uuid.New(), uuid.New()
	key := DocumentKey(tid, did)
	if key != "tenants/"+tid.String()+"/documents/"+did.String() {
		t.Fatalf("key %q", key)
	}
	gt, gd, err := ParseDocumentKey(key)
	if err != nil || gt != tid || gd != did {
		t.Fatalf("parse: %v %v %v", gt, gd, err)
	}
}

func TestParseDocumentKeyRejects(t *testing.T) {
	tid, did := uuid.New().String(), uuid.New().String()
	bad := []string{
		"",
		"tenants/" + tid,
		"tenants/" + tid + "/documents/" + did + "/x",
		"tenants/../documents/" + did,
		"tenants/" + tid + "/documents/../../etc/passwd",
		"/tenants/" + tid + "/documents/" + did,
		"tenants/" + strings.ToUpper(tid) + "/documents/" + did,
		"tenants/" + uuid.Nil.String() + "/documents/" + did,
		"other/" + tid + "/documents/" + did,
		`tenants\` + tid + `\documents\` + did,
	}
	for _, k := range bad {
		if _, _, err := ParseDocumentKey(k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%q accepted", k)
		}
	}
}

func TestFSRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := DocumentKey(uuid.New(), uuid.New())
	data := bytes.Repeat([]byte("0123456789"), 100_000)

	if err := s.Put(ctx, key, bytes.NewReader(data), -1, "text/plain"); err != nil {
		t.Fatal(err)
	}
	r, err := s.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, data) {
		t.Fatal("round trip mismatch")
	}

	// STO-AC-006: delete is idempotent.
	if err := s.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestFSFailedPutLeavesNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := NewFS(t.TempDir())
	key := DocumentKey(uuid.New(), uuid.New())
	r := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(errors.New("client went away")))
	if err := s.Put(ctx, key, r, -1, ""); err == nil {
		t.Fatal("put with failing reader succeeded")
	}
	if _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("partial object visible: %v", err)
	}
}

func TestFSRejectsBadKeysAndPresign(t *testing.T) {
	ctx := context.Background()
	s, _ := NewFS(t.TempDir())
	if err := s.Put(ctx, "../escape", strings.NewReader("x"), -1, ""); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("put bad key: %v", err)
	}
	if _, err := s.Presign(ctx, DocumentKey(uuid.New(), uuid.New()), MaxPresignTTL); !errors.Is(err, ErrPresignNotSupport) {
		t.Errorf("presign: %v", err)
	}
}
