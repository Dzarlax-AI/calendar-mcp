package httpx

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLimitRequestBody(t *testing.T) {
	h := LimitRequestBody(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			t.Error("ReadAll() returned nil error for oversized body")
		}
		w.WriteHeader(http.StatusBadRequest)
	}), 4)
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345")))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusBadRequest)
	}
}
