package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/kavish924/multi-tenant-rag/internal/documents"
	"github.com/kavish924/multi-tenant-rag/internal/httpx"
	"github.com/kavish924/multi-tenant-rag/internal/storage/objectstore"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

// multipartOverhead allows for part headers and boundaries on top of the
// file size cap.
const multipartOverhead = 1 << 20

// uploadDocument streams the multipart field "file" (DOC-REQ-001..006). The
// tenant comes only from the verified membership; any tenant-like form
// fields are ignored (DOC-AC-009).
func (s *server) uploadDocument(w http.ResponseWriter, r *http.Request) {
	m, _ := tenants.FromContext(r.Context())
	r.Body = http.MaxBytesReader(w, r.Body, s.Documents.MaxBytes()+multipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid_upload", "expected multipart/form-data with a file field")
		return
	}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			httpx.Error(w, http.StatusBadRequest, "invalid_upload", "missing file field")
			return
		}
		if err != nil {
			s.uploadError(w, r, err)
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		res, err := s.Documents.Upload(r.Context(), documents.UploadInput{
			TenantID:     m.TenantID,
			Filename:     part.FileName(),
			DeclaredType: part.Header.Get("Content-Type"),
			Body:         part,
		})
		if err != nil {
			s.uploadError(w, r, err)
			return
		}
		if res.Duplicate {
			httpx.JSON(w, http.StatusOK, map[string]any{"document": res.Document, "duplicate": true})
			return
		}
		httpx.JSON(w, http.StatusAccepted, map[string]any{"document": res.Document, "duplicate": false})
		return
	}
}

func (s *server) uploadError(w http.ResponseWriter, r *http.Request, err error) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, documents.ErrTooLarge), errors.As(err, &tooBig):
		httpx.Error(w, http.StatusRequestEntityTooLarge, "too_large",
			fmt.Sprintf("file exceeds the %d byte limit", s.Documents.MaxBytes()))
	case errors.Is(err, documents.ErrUnsupportedType):
		httpx.Error(w, http.StatusUnsupportedMediaType, "unsupported_type", err.Error())
	case errors.Is(err, documents.ErrStorage):
		s.Log.ErrorContext(r.Context(), "upload storage", "err", err)
		httpx.Error(w, http.StatusBadGateway, "storage_unavailable", "could not store the file")
	default:
		s.internal(w, r, "upload", err)
	}
}

func (s *server) listDocuments(w http.ResponseWriter, r *http.Request) {
	m, _ := tenants.FromContext(r.Context())
	q := r.URL.Query()
	p := documents.ListParams{Cursor: q.Get("cursor"), Status: q.Get("status")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			httpx.Error(w, http.StatusBadRequest, "invalid_limit", "limit must be a positive integer")
			return
		}
		p.Limit = n
	}
	if v := q.Get("include_deleted"); v != "" {
		inc, err := strconv.ParseBool(v)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid_include_deleted", "include_deleted must be true or false")
			return
		}
		if inc && !m.Role.AtLeast(tenants.RoleAdmin) {
			httpx.Forbidden(w)
			return
		}
		p.IncludeDeleted = inc
	}
	items, next, err := s.Documents.List(r.Context(), m.TenantID, p)
	switch {
	case errors.Is(err, documents.ErrBadCursor):
		httpx.Error(w, http.StatusBadRequest, "invalid_cursor", err.Error())
	case errors.Is(err, documents.ErrBadStatus):
		httpx.Error(w, http.StatusBadRequest, "invalid_status", err.Error())
	case err != nil:
		s.internal(w, r, "list documents", err)
	default:
		httpx.JSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
	}
}

func (s *server) getDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r, "id")
	if !ok {
		httpx.NotFound(w)
		return
	}
	m, _ := tenants.FromContext(r.Context())
	d, err := s.Documents.Get(r.Context(), m.TenantID, id)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		httpx.NotFound(w)
	case err != nil:
		s.internal(w, r, "get document", err)
	default:
		httpx.JSON(w, http.StatusOK, d)
	}
}

func (s *server) deleteDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r, "id")
	if !ok {
		httpx.NotFound(w)
		return
	}
	m, _ := tenants.FromContext(r.Context())
	d, err := s.Documents.Delete(r.Context(), m.TenantID, id)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		httpx.NotFound(w)
	case err != nil:
		s.internal(w, r, "delete document", err)
	default:
		httpx.JSON(w, http.StatusAccepted, d)
	}
}

// documentContent streams the original (STO-REQ-004) or, with
// ?redirect=true, redirects to a short-lived signed URL. Membership is
// verified by the middleware before any storage access (STO-AC-002).
func (s *server) documentContent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r, "id")
	if !ok {
		httpx.NotFound(w)
		return
	}
	m, _ := tenants.FromContext(r.Context())

	if redirect, _ := strconv.ParseBool(r.URL.Query().Get("redirect")); redirect {
		url, err := s.Documents.Presign(r.Context(), m.TenantID, id)
		switch {
		case errors.Is(err, documents.ErrNotFound):
			httpx.NotFound(w)
		case errors.Is(err, documents.ErrGone):
			httpx.Error(w, http.StatusGone, "deleted", err.Error())
		case errors.Is(err, objectstore.ErrPresignNotSupport):
			httpx.Error(w, http.StatusNotImplemented, "redirect_unsupported", "signed URLs are not available with this storage backend")
		case err != nil:
			s.internal(w, r, "presign", err)
		default:
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, url, http.StatusFound)
		}
		return
	}

	d, body, err := s.Documents.Open(r.Context(), m.TenantID, id)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		// Unknown/other-tenant id, or a row whose object is missing (logged:
		// an inconsistency signal, spec 003 §7).
		s.Log.WarnContext(r.Context(), "document content not found", "err", err)
		httpx.NotFound(w)
		return
	case errors.Is(err, documents.ErrGone):
		httpx.Error(w, http.StatusGone, "deleted", err.Error())
		return
	case errors.Is(err, documents.ErrStorage):
		s.Log.ErrorContext(r.Context(), "content storage", "err", err)
		httpx.Error(w, http.StatusBadGateway, "storage_unavailable", "could not read the file")
		return
	case err != nil:
		s.internal(w, r, "open document", err)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", d.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(d.SizeBytes, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": d.Filename}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, body); err != nil {
		s.Log.WarnContext(r.Context(), "content stream interrupted", "err", err)
	}
}
