package problem

import (
	"encoding/json/v2"
	"net/http/httptest"
	"testing"
)

func TestWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, Unavailable("HyperSync did not answer.", "/x", 90))
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 503 || p.Code != CodeUpstream || p.RetryAfterS != MaxRetryAfterS || rec.Header().Get("Retry-After") != "60" ||
		rec.Header().Get("Content-Type") != "application/problem+json" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("%d %+v %v", rec.Code, p, rec.Header())
	}
	if p := New("nope", "", ""); p.Code != CodeInternal || p.Status != 500 {
		t.Errorf("unknown code %+v", p)
	}
	if p := New(CodeNotFound, "x", "/y"); p.Type != "urn:sona:problem:not_found" || p.RetryAfterS != 0 {
		t.Errorf("not found %+v", p)
	}
}
