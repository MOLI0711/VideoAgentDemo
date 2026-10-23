package retrieval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRRFCombinesRanksAndDeduplicates(t *testing.T) {
	a, b, c := Document{SegmentID: "a"}, Document{SegmentID: "b"}, Document{SegmentID: "c"}
	r := Fuse(3, []Document{a, b, b}, []Document{b, c})
	if len(r) != 3 || r[0].SegmentID != "b" {
		t.Fatalf("unexpected ranking %#v", r)
	}
}
func TestLexicalFallbackMaintainsScope(t *testing.T) {
	s := Scope{"owner", "video", "version", "test", "build"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		filters := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		if len(filters) != 4 {
			t.Error("scope filters absent")
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": []any{map[string]any{"_source": Document{OwnerID: "owner", VideoID: "video", ContentVersion: "version", IndexBuildID: "build", SegmentID: "s", StartMS: 1, EndMS: 2}}}}})
	}))
	defer server.Close()
	c, _ := New(server.URL, "", 3, nil)
	result, err := c.Search(context.Background(), s, "test", 5)
	if err != nil || result.Mode != "lexical" || len(result.Documents) != 1 {
		t.Fatal(result, err)
	}
}
func TestRejectForeignEvidenceEvenInResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": []any{map[string]any{"_source": Document{OwnerID: "other"}}}}})
	}))
	defer server.Close()
	c, _ := New(server.URL, "", 3, nil)
	_, err := c.Read(context.Background(), Scope{"owner", "video", "version", "test", "build"}, []string{"x"})
	if err == nil {
		t.Fatal("foreign evidence accepted")
	}
}
