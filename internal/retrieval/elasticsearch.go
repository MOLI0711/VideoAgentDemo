package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/provider"
)

type Scope struct{ OwnerID, VideoID, ContentVersion, IndexName, IndexBuildID string }

func (s Scope) valid() bool {
	return s.OwnerID != "" && s.VideoID != "" && s.ContentVersion != "" && s.IndexName != "" && s.IndexBuildID != ""
}

type Document struct {
	OwnerID        string    `json:"owner_id"`
	VideoID        string    `json:"video_id"`
	ContentVersion string    `json:"content_version"`
	IndexBuildID   string    `json:"index_build_id"`
	SegmentID      string    `json:"segment_id"`
	StartMS        int64     `json:"start_ms"`
	EndMS          int64     `json:"end_ms"`
	Transcript     string    `json:"transcript"`
	OCRText        string    `json:"ocr_text"`
	Title          string    `json:"title"`
	FrameIDs       []string  `json:"frame_ids"`
	Embedding      []float32 `json:"embedding,omitempty"`
}
type Result struct {
	Documents []Document `json:"documents"`
	Mode      string     `json:"mode"`
}
type Embedder interface {
	Embed(context.Context, []string) (provider.EmbeddingResponse, error)
}
type Client struct {
	Endpoint, APIKey string
	Dimensions       int
	Model            Embedder
	HTTP             *http.Client
}

func New(endpoint, key string, dimensions int, model Embedder) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || dimensions < 1 {
		return nil, fmt.Errorf("invalid search configuration")
	}
	return &Client{Endpoint: strings.TrimRight(endpoint, "/"), APIKey: key, Dimensions: dimensions, Model: model, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}
func (c *Client) request(ctx context.Context, method, path string, body []byte, contentType string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", contentType)
	if c.APIKey != "" {
		req.Header.Set("Authorization", "ApiKey "+c.APIKey)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return nil, res.StatusCode, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return b, res.StatusCode, fmt.Errorf("search HTTP status %d", res.StatusCode)
	}
	return b, res.StatusCode, nil
}
func (c *Client) Ping(ctx context.Context) error {
	_, _, err := c.request(ctx, "GET", "/", nil, "application/json")
	return err
}
func (c *Client) EnsureIndex(ctx context.Context, index string) error {
	if index == "" || strings.ContainsAny(index, "/*?#") {
		return errors.New("invalid index name")
	}
	_, status, err := c.request(ctx, "HEAD", "/"+url.PathEscape(index), nil, "application/json")
	if err == nil {
		return c.verifyMapping(ctx, index)
	}
	if status != 404 {
		return err
	}
	props := map[string]any{}
	for _, k := range []string{"owner_id", "video_id", "content_version", "index_build_id", "segment_id", "frame_ids"} {
		props[k] = map[string]string{"type": "keyword"}
	}
	for _, k := range []string{"transcript", "ocr_text", "title"} {
		props[k] = map[string]string{"type": "text", "analyzer": "cjk"}
	}
	props["start_ms"], props["end_ms"] = map[string]string{"type": "long"}, map[string]string{"type": "long"}
	props["embedding"] = map[string]any{"type": "dense_vector", "dims": c.Dimensions, "index": true, "similarity": "cosine"}
	b, _ := json.Marshal(map[string]any{"mappings": map[string]any{"dynamic": "strict", "properties": props}})
	_, _, err = c.request(ctx, "PUT", "/"+url.PathEscape(index), b, "application/json")
	if err != nil {
		if verified := c.verifyMapping(ctx, index); verified == nil {
			return nil
		}
	}
	return err
}
func (c *Client) verifyMapping(ctx context.Context, index string) error {
	b, _, err := c.request(ctx, "GET", "/"+url.PathEscape(index)+"/_mapping", nil, "application/json")
	if err != nil {
		return err
	}
	var all map[string]struct {
		Mappings struct {
			Properties map[string]struct {
				Type string `json:"type"`
				Dims int    `json:"dims"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err = json.Unmarshal(b, &all); err != nil {
		return err
	}
	m, ok := all[index]
	if !ok || m.Mappings.Properties["embedding"].Type != "dense_vector" || m.Mappings.Properties["embedding"].Dims != c.Dimensions {
		return errors.New("existing index embedding mapping differs; use a new index/profile")
	}
	for _, name := range []string{"owner_id", "video_id", "content_version", "index_build_id", "segment_id"} {
		if m.Mappings.Properties[name].Type != "keyword" {
			return errors.New("existing index scope mapping is not keyword")
		}
	}
	return nil
}
func (c *Client) validVector(v []float32) bool {
	if len(v) != c.Dimensions {
		return false
	}
	norm := float64(0)
	for _, n := range v {
		if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
			return false
		}
		norm += float64(n) * float64(n)
	}
	return norm > 0
}
func (c *Client) Build(ctx context.Context, s Scope, docs []Document) error {
	if !s.valid() || len(docs) == 0 || c.Model == nil {
		return errors.New("index build requires scope, documents and embedding model")
	}
	if err := c.EnsureIndex(ctx, s.IndexName); err != nil {
		return err
	}
	for start := 0; start < len(docs); start += 16 {
		end := min(start+16, len(docs))
		batch := docs[start:end]
		texts := make([]string, len(batch))
		for i, d := range batch {
			if d.SegmentID == "" || d.EndMS <= d.StartMS {
				return errors.New("invalid segment")
			}
			texts[i] = d.Transcript + "\n[OCR]\n" + d.OCRText
		}
		emb, err := c.Model.Embed(ctx, texts)
		if err != nil {
			return err
		}
		if len(emb.Vectors) != len(batch) {
			return errors.New("embedding result count mismatch")
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for i, d := range batch {
			if !c.validVector(emb.Vectors[i]) {
				return errors.New("embedding dimension or value mismatch")
			}
			d.OwnerID, d.VideoID, d.ContentVersion, d.IndexBuildID = s.OwnerID, s.VideoID, s.ContentVersion, s.IndexBuildID
			d.Embedding = emb.Vectors[i]
			_ = enc.Encode(map[string]any{"index": map[string]string{"_index": s.IndexName, "_id": s.VideoID + ":" + s.ContentVersion + ":" + s.IndexBuildID + ":" + d.SegmentID}})
			_ = enc.Encode(d)
		}
		b, _, err := c.request(ctx, "POST", "/_bulk", buf.Bytes(), "application/x-ndjson")
		if err != nil {
			return err
		}
		var response struct {
			Errors bool `json:"errors"`
			Items  []map[string]struct {
				Status int `json:"status"`
			} `json:"items"`
		}
		if err = json.Unmarshal(b, &response); err != nil {
			return err
		}
		if response.Errors || len(response.Items) != len(batch) {
			return errors.New("partial bulk failure; build remains unpublished")
		}
		for _, item := range response.Items {
			for _, v := range item {
				if v.Status < 200 || v.Status >= 300 {
					return errors.New("bulk item failed")
				}
			}
		}
	}
	_, _, err := c.request(ctx, "POST", "/"+url.PathEscape(s.IndexName)+"/_refresh", nil, "application/json")
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"query": map[string]any{"bool": map[string]any{"filter": filters(s)}}})
	raw, _, err := c.request(ctx, "POST", "/"+url.PathEscape(s.IndexName)+"/_count", b, "application/json")
	if err != nil {
		return err
	}
	var count struct {
		Count int `json:"count"`
	}
	if err = json.Unmarshal(raw, &count); err != nil {
		return err
	}
	if count.Count != len(docs) {
		return errors.New("searchable build count mismatch")
	}
	return nil
}
func filters(s Scope) []any {
	return []any{map[string]any{"term": map[string]string{"owner_id": s.OwnerID}}, map[string]any{"term": map[string]string{"video_id": s.VideoID}}, map[string]any{"term": map[string]string{"content_version": s.ContentVersion}}, map[string]any{"term": map[string]string{"index_build_id": s.IndexBuildID}}}
}
func (c *Client) query(ctx context.Context, s Scope, q any) ([]Document, error) {
	b, _ := json.Marshal(q)
	raw, _, err := c.request(ctx, "POST", "/"+url.PathEscape(s.IndexName)+"/_search", b, "application/json")
	if err != nil {
		return nil, err
	}
	var result struct {
		Hits struct {
			Hits []struct {
				Source Document `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	out := make([]Document, 0, len(result.Hits.Hits))
	for _, h := range result.Hits.Hits {
		d := h.Source
		if d.OwnerID != s.OwnerID || d.VideoID != s.VideoID || d.ContentVersion != s.ContentVersion || d.IndexBuildID != s.IndexBuildID {
			return nil, errors.New("search returned evidence outside scope")
		}
		d.Embedding = nil
		out = append(out, d)
	}
	return out, nil
}
func (c *Client) Search(ctx context.Context, s Scope, query string, limit int) (Result, error) {
	if !s.valid() || strings.TrimSpace(query) == "" || len(query) > 4096 {
		return Result{}, errors.New("invalid search input")
	}
	limit = max(1, min(12, limit))
	var lex, vec []Document
	var le, ve error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		lex, le = c.query(ctx, s, map[string]any{"size": 30, "query": map[string]any{"bool": map[string]any{"filter": filters(s), "must": []any{map[string]any{"multi_match": map[string]any{"query": query, "fields": []string{"transcript^2", "ocr_text", "title"}}}}}}})
	}()
	go func() {
		defer wg.Done()
		if c.Model == nil {
			ve = errors.New("embedding unavailable")
			return
		}
		emb, err := c.Model.Embed(ctx, []string{query})
		if err != nil {
			ve = err
			return
		}
		if len(emb.Vectors) != 1 || !c.validVector(emb.Vectors[0]) {
			ve = errors.New("query embedding mismatch")
			return
		}
		vec, ve = c.query(ctx, s, map[string]any{"size": 30, "knn": map[string]any{"field": "embedding", "query_vector": emb.Vectors[0], "k": 30, "num_candidates": 100, "filter": map[string]any{"bool": map[string]any{"filter": filters(s)}}}})
	}()
	wg.Wait()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if le != nil {
		return Result{}, le
	}
	if ve != nil {
		return Result{Documents: Fuse(limit, lex), Mode: "lexical"}, nil
	}
	return Result{Documents: Fuse(limit, lex, vec), Mode: "hybrid"}, nil
}
func (c *Client) Read(ctx context.Context, s Scope, ids []string) ([]Document, error) {
	if !s.valid() || len(ids) == 0 || len(ids) > 12 {
		return nil, errors.New("invalid evidence selection")
	}
	f := append(filters(s), map[string]any{"terms": map[string]any{"segment_id": ids}})
	return c.query(ctx, s, map[string]any{"size": len(ids), "query": map[string]any{"bool": map[string]any{"filter": f}}})
}
func Fuse(limit int, lists ...[]Document) []Document {
	type ranked struct {
		doc   Document
		score float64
	}
	m := map[string]*ranked{}
	for _, list := range lists {
		seen := map[string]bool{}
		for rank, d := range list {
			if seen[d.SegmentID] {
				continue
			}
			seen[d.SegmentID] = true
			r := m[d.SegmentID]
			if r == nil {
				r = &ranked{doc: d}
				m[d.SegmentID] = r
			}
			r.score += 1 / float64(61+rank)
		}
	}
	all := make([]*ranked, 0, len(m))
	for _, r := range m {
		all = append(all, r)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].score == all[j].score {
			return all[i].doc.SegmentID < all[j].doc.SegmentID
		}
		return all[i].score > all[j].score
	})
	out := make([]Document, 0, min(limit, len(all)))
	for _, r := range all {
		if len(out) >= limit {
			break
		}
		out = append(out, r.doc)
	}
	return out
}
