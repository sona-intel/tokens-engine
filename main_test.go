package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	if err := run(t.Context(), []string{"nope"}); !errors.Is(err, errUsage) {
		t.Errorf("got %v", err)
	}
}

func TestHealthcheck(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer ok.Close()
	if err := healthcheck(t.Context(), ok.URL); err != nil {
		t.Errorf("healthy: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer bad.Close()
	if err := healthcheck(t.Context(), bad.URL); err == nil {
		t.Error("a 503 passed")
	}
}
