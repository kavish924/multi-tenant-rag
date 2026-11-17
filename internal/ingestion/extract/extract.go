// Package extract converts PDF, DOCX and plain-text files into text
// sections with page and heading metadata (ING-REQ-001). Parsers are an
// attack surface, so production code runs them in a separate, resource-
// limited process (isolate.go, ING-REQ-002); the functions here are what
// that process executes.
package extract

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// Content types (kept in sync with internal/documents).
const (
	TypePDF  = "application/pdf"
	TypeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	TypeText = "text/plain"
)

// Section is a run of text that shares a page and heading. Paragraphs inside
// Text are separated by blank lines.
type Section struct {
	Page    int    `json:"page,omitempty"` // 1-based; 0 when the format has no pages
	Heading string `json:"heading,omitempty"`
	Text    string `json:"text"`
}

// Errors. ErrLimit and ErrMalformed are permanent: retrying the same bytes
// cannot succeed.
var (
	ErrUnsupported = errors.New("extract: unsupported content type")
	ErrMalformed   = errors.New("extract: malformed document")
	ErrLimit       = errors.New("extract: parser_limit_exceeded")
)

// Limits bound what a parser may consume.
type Limits struct {
	MaxInputBytes  int64 // file size accepted by the helper
	MaxExpandBytes int64 // decompressed size (DOCX zip entries)
	MaxOutputBytes int64 // extracted text
	MaxZipEntries  int
	MaxPDFPages    int
}

// DefaultLimits suit the 50 MB upload cap.
var DefaultLimits = Limits{
	MaxInputBytes:  64 << 20,
	MaxExpandBytes: 256 << 20,
	MaxOutputBytes: 32 << 20,
	MaxZipEntries:  10_000,
	MaxPDFPages:    5_000,
}

// Extract dispatches on content type. It must only run inside the isolated
// helper process in production.
func Extract(contentType string, data []byte, lim Limits) ([]Section, error) {
	if int64(len(data)) > lim.MaxInputBytes {
		return nil, fmt.Errorf("%w: input larger than %d bytes", ErrLimit, lim.MaxInputBytes)
	}
	var (
		secs []Section
		err  error
	)
	switch contentType {
	case TypeText:
		secs, err = extractText(data)
	case TypeDOCX:
		secs, err = extractDOCX(data, lim)
	case TypePDF:
		secs, err = extractPDF(data, lim)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, contentType)
	}
	if err != nil {
		return nil, err
	}
	var total int64
	out := secs[:0]
	for _, s := range secs {
		s.Text = normalize(s.Text)
		s.Heading = strings.TrimSpace(collapseSpaces(s.Heading))
		if s.Text == "" {
			continue
		}
		total += int64(len(s.Text))
		if total > lim.MaxOutputBytes {
			return nil, fmt.Errorf("%w: extracted text larger than %d bytes", ErrLimit, lim.MaxOutputBytes)
		}
		out = append(out, s)
	}
	return out, nil
}

// extractText handles UTF-8 text; Markdown-style "# " lines become headings.
func extractText(data []byte) ([]Section, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.ToValidUTF8(string(data), "�")
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")

	var secs []Section
	cur := Section{}
	var b strings.Builder
	flush := func() {
		cur.Text = b.String()
		if strings.TrimSpace(cur.Text) != "" {
			secs = append(secs, cur)
		}
		b.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if h, ok := markdownHeading(line); ok {
			flush()
			cur = Section{Heading: h}
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return secs, nil
}

func markdownHeading(line string) (string, bool) {
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, "#") {
		return "", false
	}
	level := len(t) - len(strings.TrimLeft(t, "#"))
	if level > 6 || len(t) == level || t[level] != ' ' {
		return "", false
	}
	return strings.TrimSpace(t[level:]), true
}

// normalize trims trailing spaces, collapses runs of blank lines into one
// paragraph break, and drops control characters.
func normalize(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	var b strings.Builder
	blank := 0
	for _, l := range lines {
		l = strings.TrimRight(collapseSpaces(l), " ")
		if strings.TrimSpace(l) == "" {
			blank++
			continue
		}
		if b.Len() > 0 {
			if blank > 0 {
				b.WriteString("\n\n")
			} else {
				b.WriteByte('\n')
			}
		}
		blank = 0
		b.WriteString(strings.TrimLeft(l, " "))
	}
	return b.String()
}

func collapseSpaces(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return s
}
