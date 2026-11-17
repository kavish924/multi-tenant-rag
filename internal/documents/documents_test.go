package documents

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDetect(t *testing.T) {
	cases := map[string]struct {
		head []byte
		want string
	}{
		"pdf":           {[]byte("%PDF-1.7\n%âãÏÓ"), TypePDF},
		"docx":          {[]byte("PK\x03\x04\x14\x00\x06\x00"), TypeDOCX},
		"ascii":         {[]byte("Refund policy\nCustomers may return items within 30 days."), TypeText},
		"utf8 bom":      {[]byte("\xef\xbb\xbfhello"), TypeText},
		"utf8 cut rune": {[]byte("caf\xc3"), TypeText},
		"tabs+crlf":     {[]byte("a\tb\r\nc"), TypeText},
		"binary nul":    {[]byte("abc\x00def"), ""},
		"invalid utf8":  {[]byte("\xff\xfe\xfd\xfc bad"), ""},
		"png":           {[]byte("\x89PNG\r\n\x1a\n"), ""},
		"elf":           {[]byte("\x7fELF\x02\x01\x01"), ""},
		"empty":         {nil, ""},
	}
	for name, c := range cases {
		if got := Detect(c.head); got != c.want {
			t.Errorf("%s: Detect = %q, want %q", name, got, c.want)
		}
	}
}

func TestNormalizeDeclared(t *testing.T) {
	ok := map[string]string{
		"application/pdf":           TypePDF,
		"Application/PDF":           TypePDF,
		"text/plain; charset=utf-8": TypeText,
		" " + TypeDOCX:              TypeDOCX,
	}
	for in, want := range ok {
		if got, valid := NormalizeDeclared(in); !valid || got != want {
			t.Errorf("%q → %q %v, want %q", in, got, valid, want)
		}
	}
	for _, bad := range []string{"", "application/octet-stream", "text/html", "image/png", ";;;"} {
		if _, valid := NormalizeDeclared(bad); valid {
			t.Errorf("%q accepted", bad)
		}
	}
}

// DOC-AC-001: the limit reader aborts as soon as the cap is exceeded.
func TestLimitReader(t *testing.T) {
	read := func(size, limit int) error {
		lr := &limitReader{r: bytes.NewReader(make([]byte, size)), remaining: int64(limit)}
		_, err := io.Copy(io.Discard, lr)
		return err
	}
	if err := read(100, 100); err != nil {
		t.Errorf("exactly at limit: %v", err)
	}
	if err := read(101, 100); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one byte over: %v", err)
	}
	if err := read(10_000_000, 1000); !errors.Is(err, ErrTooLarge) {
		t.Errorf("far over: %v", err)
	}
}

func TestCleanFilename(t *testing.T) {
	cases := map[string]string{
		"report.pdf":             "report.pdf",
		"../../etc/passwd":       "passwd",
		`C:\Users\x\secret.docx`: "secret.docx",
		"a\x00b\nc\".txt":        "abc.txt",
		"":                       "upload",
		"..":                     "upload",
		"/":                      "upload",
		strings.Repeat("é", 300): strings.Repeat("é", 255),
	}
	for in, want := range cases {
		if got := cleanFilename(in); got != want {
			t.Errorf("cleanFilename(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 11, 16, 10, 30, 0, 123456000, time.UTC)
	id := uuid.New()
	gotT, gotID, err := decodeCursor(encodeCursor(ts, id))
	if err != nil || !gotT.Equal(ts) || gotID != id {
		t.Fatalf("round trip: %v %v %v", gotT, gotID, err)
	}
	for _, bad := range []string{"%%%", "bm90LWEtY3Vyc29y"} {
		if _, _, err := decodeCursor(bad); err == nil {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}
