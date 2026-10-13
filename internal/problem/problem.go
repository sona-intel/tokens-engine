package problem

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strconv"
)

type Problem struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Status      int    `json:"status"`
	Detail      string `json:"detail,omitzero"`
	Instance    string `json:"instance,omitzero"`
	Code        string `json:"code"`
	RetryAfterS int    `json:"retry_after_s,omitzero"`
}

const (
	CodeInvalidQuery     = "invalid_query"
	CodeInvalidID        = "invalid_id"
	CodeUnsupportedChain = "unsupported_chain"
	CodeNotFound         = "not_found"
	CodeInternal         = "internal"
	CodeUpstream         = "upstream_unavailable"
	CodeDeadline         = "deadline_exceeded"
)

// MaxRetryAfterS caps the hint, since a source that pauses longer is better retried by a caller's own schedule
const MaxRetryAfterS = 60

var kinds = map[string]struct {
	status int
	title  string
}{
	CodeInvalidQuery:     {http.StatusBadRequest, "Invalid query"},
	CodeInvalidID:        {http.StatusBadRequest, "Invalid identifier"},
	CodeUnsupportedChain: {http.StatusNotFound, "Unsupported chain"},
	CodeNotFound:         {http.StatusNotFound, "Not found"},
	CodeInternal:         {http.StatusInternalServerError, "Internal error"},
	CodeUpstream:         {http.StatusServiceUnavailable, "Data source unavailable"},
	CodeDeadline:         {http.StatusGatewayTimeout, "Deadline exceeded"},
}

func New(code, detail, instance string) Problem {
	kind, ok := kinds[code]
	if !ok {
		code = CodeInternal
		kind = kinds[code]
	}
	p := Problem{
		Type:     "urn:sona:problem:" + code,
		Title:    kind.title,
		Status:   kind.status,
		Detail:   detail,
		Instance: instance,
		Code:     code,
	}
	if code == CodeUpstream {
		p.RetryAfterS = 1
	}
	return p
}

// Unavailable is upstream_unavailable with a retry hint of 1 to MaxRetryAfterS seconds
func Unavailable(detail, instance string, retryAfterS int) Problem {
	p := New(CodeUpstream, detail, instance)
	p.RetryAfterS = min(max(retryAfterS, 1), MaxRetryAfterS)
	return p
}

func Write(w http.ResponseWriter, p Problem) {
	// AllowInvalidUTF8 leaves no encode error to handle
	body, _ := json.Marshal(p, jsontext.AllowInvalidUTF8(true))
	h := w.Header()
	h.Set("Content-Type", "application/problem+json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	if p.RetryAfterS > 0 {
		h.Set("Retry-After", strconv.Itoa(p.RetryAfterS))
	}
	w.WriteHeader(p.Status)
	_, _ = w.Write(body)
}
