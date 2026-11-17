package extract

import (
	"bytes"
	"fmt"

	"github.com/ledongthuc/pdf"
)

// extractPDF returns one section per page. PDFs carry no reliable heading
// structure, so Heading stays empty. The library can panic on hostile
// input; the isolated helper process contains that, and the recover here
// turns it into ErrMalformed when called in-process (tests).
func extractPDF(data []byte, lim Limits) (secs []Section, err error) {
	defer func() {
		if p := recover(); p != nil {
			secs, err = nil, fmt.Errorf("%w: pdf parser panic: %v", ErrMalformed, p)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	n := r.NumPage()
	if n <= 0 {
		return nil, fmt.Errorf("%w: no pages", ErrMalformed)
	}
	if n > lim.MaxPDFPages {
		return nil, fmt.Errorf("%w: %d pages", ErrLimit, n)
	}
	var total int64
	for i := 1; i <= n; i++ {
		text, err := r.Page(i).GetPlainText(nil)
		if err != nil {
			return nil, fmt.Errorf("%w: page %d: %v", ErrMalformed, i, err)
		}
		total += int64(len(text))
		if total > lim.MaxOutputBytes {
			return nil, fmt.Errorf("%w: extracted text larger than %d bytes", ErrLimit, lim.MaxOutputBytes)
		}
		secs = append(secs, Section{Page: i, Text: text})
	}
	return secs, nil
}
