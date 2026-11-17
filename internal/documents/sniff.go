package documents

import (
	"bytes"
	"mime"
	"strings"
	"unicode/utf8"
)

// Allowed content types (DOC-REQ-001).
const (
	TypePDF  = "application/pdf"
	TypeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	TypeText = "text/plain"
)

// SniffLen is how many leading bytes Detect inspects.
const SniffLen = 512

// Detect classifies a file by its leading bytes (DOC-REQ-002). It returns ""
// for anything that is not a PDF, a ZIP container (DOCX), or UTF-8 text.
func Detect(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return TypePDF
	case bytes.HasPrefix(head, []byte("PK\x03\x04")):
		return TypeDOCX
	case looksLikeText(head):
		return TypeText
	default:
		return ""
	}
}

func looksLikeText(head []byte) bool {
	if len(head) == 0 {
		return false
	}
	head = bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	// The sniff window may cut a multi-byte rune; drop up to 3 trailing bytes.
	for i := 0; i < 3 && len(head) > 0 && !utf8.Valid(head); i++ {
		head = head[:len(head)-1]
	}
	if !utf8.Valid(head) {
		return false
	}
	for _, b := range head {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' {
			return false // NUL and other control bytes: binary
		}
	}
	return true
}

// NormalizeDeclared parses a declared Content-Type and returns the bare,
// lower-case media type if it is one of the allowed types.
func NormalizeDeclared(ct string) (string, bool) {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(ct))
	if err != nil {
		return "", false
	}
	switch mt {
	case TypePDF, TypeDOCX, TypeText:
		return mt, true
	default:
		return "", false
	}
}
