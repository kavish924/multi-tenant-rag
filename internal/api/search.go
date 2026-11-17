package api

import (
	"errors"
	"net/http"

	"github.com/kavish924/multi-tenant-rag/internal/httpx"
	"github.com/kavish924/multi-tenant-rag/internal/retrieval"
	"github.com/kavish924/multi-tenant-rag/internal/tenants"
)

type searchRequest struct {
	Question string `json:"question"`
	Limit    int    `json:"limit"`
}

// search runs hybrid retrieval in the verified tenant (spec 007). It is the
// retrieval half of /ask (008), exposed for debugging and evaluation (014).
func (s *server) search(w http.ResponseWriter, r *http.Request) {
	m, _ := tenants.FromContext(r.Context())
	var req searchRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	if req.Limit < 0 || req.Limit > retrieval.MaxLimit {
		httpx.Error(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
		return
	}
	hits, err := s.Retriever.Search(r.Context(), m.TenantID, req.Question, req.Limit)
	switch {
	case errors.Is(err, retrieval.ErrEmptyQuestion), errors.Is(err, retrieval.ErrQuestionTooLong):
		httpx.Error(w, http.StatusBadRequest, "invalid_question", err.Error())
	case errors.Is(err, retrieval.ErrEmbeddingFailure):
		s.Log.Error("search: embedding", "err", err)
		httpx.Error(w, http.StatusBadGateway, "upstream_unavailable", "the embedding provider is unavailable")
	case err != nil:
		s.Log.Error("search", "err", err)
		httpx.Internal(w)
	default:
		if hits == nil {
			hits = []retrieval.Hit{}
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"hits": hits})
	}
}
