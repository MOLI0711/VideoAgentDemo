package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MOLI0711/MOLIVedio/internal/config"
)

func TestStorageCORSProbeUsesInternalEndpoint(t *testing.T) {
	for _, allowed := range []string{"http://127.0.0.1:5173", "*", "https://wrong.invalid", ""} {
		t.Run(allowed, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "OPTIONS" || r.URL.Path != "/test-bucket" || r.Header.Get("Origin") != "http://127.0.0.1:5173" || r.Header.Get("Access-Control-Request-Method") != "PUT" {
					t.Error("incorrect preflight request")
				}
				w.Header().Set("Access-Control-Allow-Origin", allowed)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			c := config.Default()
			c.App.PublicOrigin = "http://127.0.0.1:5173"
			c.Storage.InternalEndpoint = server.URL
			c.Storage.BrowserEndpoint = "http://127.0.0.1:1"
			c.Storage.Bucket = "test-bucket"
			err := verifyStorageCORS(context.Background(), c)
			valid := allowed == "*" || allowed == c.App.PublicOrigin
			if (err == nil) != valid {
				t.Fatalf("allowed origin %q, got error %v", allowed, err)
			}
		})
	}
}
