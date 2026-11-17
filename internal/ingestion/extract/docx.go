package extract

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// extractDOCX streams word/document.xml. Zip-bomb defenses: entry count cap,
// declared and actual decompressed size caps (DefaultLimits).
func extractDOCX(data []byte, lim Limits) ([]Section, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: not a zip archive: %v", ErrMalformed, err)
	}
	if len(zr.File) > lim.MaxZipEntries {
		return nil, fmt.Errorf("%w: %d zip entries", ErrLimit, len(zr.File))
	}
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return nil, fmt.Errorf("%w: word/document.xml missing", ErrMalformed)
	}
	if doc.UncompressedSize64 > uint64(lim.MaxExpandBytes) { //nolint:gosec // limit is positive
		return nil, fmt.Errorf("%w: document.xml declares %d bytes", ErrLimit, doc.UncompressedSize64)
	}
	rc, err := doc.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	defer rc.Close()
	// The declared size can lie; cap what is actually inflated.
	lr := &capReader{r: rc, remaining: lim.MaxExpandBytes}
	secs, err := parseDocumentXML(lr)
	if lr.exceeded {
		return nil, fmt.Errorf("%w: document.xml inflates past %d bytes", ErrLimit, lim.MaxExpandBytes)
	}
	return secs, err
}

// parseDocumentXML walks paragraphs (w:p) collecting text runs (w:t),
// tabs and breaks. Paragraph styles named Heading*/Title start a new
// section; explicit page breaks and Word's last-rendered page breaks
// advance the page counter.
func parseDocumentXML(r io.Reader) ([]Section, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = true
	const w = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"

	var (
		secs    []Section
		cur     = Section{Page: 1}
		body    strings.Builder
		para    strings.Builder
		style   string
		inText  bool
		inPara  bool
		page    = 1
		pending int // page breaks seen inside the current paragraph
	)
	flushSection := func() {
		cur.Text = body.String()
		if strings.TrimSpace(cur.Text) != "" {
			secs = append(secs, cur)
		}
		body.Reset()
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: document.xml: %v", ErrMalformed, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != w {
				continue
			}
			switch t.Name.Local {
			case "p":
				inPara, style, pending = true, "", 0
				para.Reset()
			case "pStyle":
				for _, a := range t.Attr {
					if a.Name.Local == "val" {
						style = a.Value
					}
				}
			case "t":
				inText = true
			case "tab":
				para.WriteByte('\t')
			case "br", "cr":
				isPage := false
				for _, a := range t.Attr {
					if a.Name.Local == "type" && a.Value == "page" {
						isPage = true
					}
				}
				if isPage {
					pending++
				} else {
					para.WriteByte('\n')
				}
			case "lastRenderedPageBreak":
				pending++
			}
		case xml.CharData:
			if inText {
				para.Write(t)
			}
		case xml.EndElement:
			if t.Name.Space != w {
				continue
			}
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				if !inPara {
					continue
				}
				inPara = false
				text := strings.TrimSpace(para.String())
				if pending > 0 {
					// Text after a page break belongs to the next page; keep
					// it simple and attribute the whole paragraph to it.
					flushSection()
					page += pending
					cur = Section{Page: page, Heading: cur.Heading}
				}
				if isHeadingStyle(style) && text != "" {
					flushSection()
					cur = Section{Page: page, Heading: text}
					continue
				}
				if text != "" {
					body.WriteString(text)
					body.WriteString("\n\n")
				}
			}
		}
	}
	flushSection()
	return secs, nil
}

func isHeadingStyle(style string) bool {
	s := strings.ToLower(style)
	return strings.HasPrefix(s, "heading") || s == "title"
}

type capReader struct {
	r         io.Reader
	remaining int64
	exceeded  bool
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		c.exceeded = true
		return 0, ErrLimit
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}
