package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain doubles as the helper process when re-executed by Isolated.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		HelperMain()
	}
	os.Exit(m.Run())
}

func TestText(t *testing.T) {
	in := "\xef\xbb\xbfIntro line\r\n\r\n\r\n# Refunds\nItems  may be returned\twithin 30 days.\n\nKeep the receipt.\n## Not a heading?\nx\n#nospace stays text\n"
	secs, err := Extract(TypeText, []byte(in), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	want := []Section{
		{Text: "Intro line"},
		{Heading: "Refunds", Text: "Items may be returned within 30 days.\n\nKeep the receipt."},
		{Heading: "Not a heading?", Text: "x\n#nospace stays text"},
	}
	if fmt.Sprint(secs) != fmt.Sprint(want) {
		t.Fatalf("got %#v\nwant %#v", secs, want)
	}
}

func TestTextInvalidUTF8IsReplaced(t *testing.T) {
	secs, err := Extract(TypeText, []byte("caf\xff ok"), DefaultLimits)
	if err != nil || len(secs) != 1 || !strings.Contains(secs[0].Text, "�") {
		t.Fatalf("%v %v", secs, err)
	}
}

// docx builds a minimal DOCX with the given document.xml body.
func docx(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("[Content_Types].xml")
	_, _ = w.Write([]byte(`<?xml version="1.0"?><Types/>`))
	w, _ = zw.Create("word/document.xml")
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>%s</w:body></w:document>`, body)
	_ = zw.Close()
	return buf.Bytes()
}

func para(style, text string) string {
	ps := ""
	if style != "" {
		ps = `<w:pPr><w:pStyle w:val="` + style + `"/></w:pPr>`
	}
	return `<w:p>` + ps + `<w:r><w:t xml:space="preserve">` + text + `</w:t></w:r></w:p>`
}

func TestDOCX(t *testing.T) {
	body := para("Title", "Handbook") +
		para("", "Welcome to the team.") +
		para("Heading1", "Leave policy") +
		para("", "You get 20 days.") +
		`<w:p><w:r><w:br w:type="page"/><w:t>Carry over up to 5 days.</w:t></w:r></w:p>` +
		para("Heading2", "Sick leave") +
		`<w:p><w:r><w:t>Tell your</w:t><w:tab/><w:t>manager.</w:t></w:r></w:p>`
	secs, err := Extract(TypeDOCX, docx(t, body), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	want := []Section{
		{Page: 1, Heading: "Handbook", Text: "Welcome to the team."},
		{Page: 1, Heading: "Leave policy", Text: "You get 20 days."},
		{Page: 2, Heading: "Leave policy", Text: "Carry over up to 5 days."},
		{Page: 2, Heading: "Sick leave", Text: "Tell your manager."},
	}
	if fmt.Sprint(secs) != fmt.Sprint(want) {
		t.Fatalf("got  %#v\nwant %#v", secs, want)
	}
}

func TestDOCXHostile(t *testing.T) {
	lim := DefaultLimits
	lim.MaxExpandBytes = 64 << 10

	// Decompression bomb: tiny zip, huge inflated document.xml.
	bomb := docx(t, strings.Repeat(para("", strings.Repeat("A", 1000)), 200))
	if _, err := Extract(TypeDOCX, bomb, lim); !errors.Is(err, ErrLimit) {
		t.Errorf("bomb: %v, want ErrLimit", err)
	}

	cases := map[string][]byte{
		"not a zip": []byte("PK\x03\x04 definitely not a zip"),
		"no document": func() []byte {
			var b bytes.Buffer
			zw := zip.NewWriter(&b)
			_, _ = zw.Create("x")
			_ = zw.Close()
			return b.Bytes()
		}(),
		"broken xml": docx(t, `<w:p><w:r><w:t>unclosed`),
	}
	for name, data := range cases {
		if _, err := Extract(TypeDOCX, data, DefaultLimits); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v, want ErrMalformed", name, err)
		}
	}

	many := func() []byte {
		var b bytes.Buffer
		zw := zip.NewWriter(&b)
		for i := 0; i < 50; i++ {
			_, _ = zw.Create(fmt.Sprintf("f%d", i))
		}
		_ = zw.Close()
		return b.Bytes()
	}()
	lim2 := DefaultLimits
	lim2.MaxZipEntries = 10
	if _, err := Extract(TypeDOCX, many, lim2); !errors.Is(err, ErrLimit) {
		t.Errorf("too many entries: %v", err)
	}
}

// makePDF writes a minimal valid PDF with one text line per page.
func makePDF(pages ...string) []byte {
	var objs []string
	n := len(pages)
	kids := ""
	for i := 0; i < n; i++ {
		kids += fmt.Sprintf("%d 0 R ", 4+2*i)
	}
	objs = append(objs,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica /Encoding /WinAnsiEncoding >>",
	)
	for i, text := range pages {
		stream := fmt.Sprintf("BT /F1 12 Tf 72 712 Td (%s) Tj ET", text)
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		)
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestPDF(t *testing.T) {
	secs, err := Extract(TypePDF, makePDF("Refunds within thirty days", "Keep your receipt"), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 2 || secs[0].Page != 1 || secs[1].Page != 2 ||
		!strings.Contains(secs[0].Text, "Refunds within thirty days") || !strings.Contains(secs[1].Text, "Keep your receipt") {
		t.Fatalf("sections %#v", secs)
	}
}

func TestPDFMalformedDoesNotPanic(t *testing.T) {
	for name, data := range map[string][]byte{
		"garbage":   []byte("%PDF-1.4\ngarbage garbage"),
		"truncated": makePDF("hello")[:120],
	} {
		if _, err := Extract(TypePDF, data, DefaultLimits); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestUnsupported(t *testing.T) {
	if _, err := Extract("image/png", []byte("x"), DefaultLimits); !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
}

func isolated(t *testing.T) *Isolated {
	t.Helper()
	return &Isolated{Command: []string{os.Args[0], HelperArg}, Timeout: time.Minute}
}

func TestIsolatedRoundTrip(t *testing.T) {
	x := isolated(t)
	secs, err := x.Extract(context.Background(), TypePDF, bytes.NewReader(makePDF("Isolated page")))
	if err != nil {
		t.Fatal(err)
	}
	if len(secs) != 1 || !strings.Contains(secs[0].Text, "Isolated page") {
		t.Fatalf("%#v", secs)
	}
}

// ING-AC-002, ING-AC-007: hostile inputs fail with a reason; the caller
// keeps running.
func TestIsolatedFailures(t *testing.T) {
	ctx := context.Background()

	if _, err := isolated(t).Extract(ctx, TypeDOCX, bytes.NewReader([]byte("PK\x03\x04junk"))); !errors.Is(err, ErrMalformed) {
		t.Errorf("malformed docx: %v", err)
	}
	if _, err := isolated(t).Extract(ctx, "image/png", strings.NewReader("x")); !errors.Is(err, ErrUnsupported) {
		t.Errorf("unsupported: %v", err)
	}

	small := isolated(t)
	small.Limits = DefaultLimits
	small.Limits.MaxInputBytes = 1000
	if _, err := small.Extract(ctx, TypeText, strings.NewReader(strings.Repeat("a", 5000))); !errors.Is(err, ErrLimit) {
		t.Errorf("input cap: %v", err)
	}

	out := isolated(t)
	out.Limits = DefaultLimits
	out.Limits.MaxOutputBytes = 1 << 10
	if _, err := out.Extract(ctx, TypeText, strings.NewReader(strings.Repeat("word ", 40_000))); !errors.Is(err, ErrLimit) {
		t.Errorf("output cap: %v", err)
	}

	slow := isolated(t)
	slow.Timeout = time.Millisecond
	if _, err := slow.Extract(ctx, TypeText, strings.NewReader("hello")); !errors.Is(err, ErrLimit) {
		t.Errorf("timeout: %v", err)
	}
}
