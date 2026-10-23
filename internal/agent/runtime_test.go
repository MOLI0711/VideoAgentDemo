package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	"github.com/MOLI0711/MOLIVedio/internal/retrieval"
	"testing"
)

type testSearch struct{ calls int }

func (s *testSearch) Search(ctx context.Context, scope retrieval.Scope, q string, n int) (retrieval.Result, error) {
	s.calls++
	return retrieval.Result{Documents: []retrieval.Document{{SegmentID: "s1", StartMS: 0, EndMS: 1000, Transcript: "Synthetic fixture: the upload is processed asynchronously."}}, Mode: "fixture"}, nil
}
func (s *testSearch) Read(ctx context.Context, scope retrieval.Scope, ids []string) ([]retrieval.Document, error) {
	res, e := s.Search(ctx, scope, "", 0)
	return res.Documents, e
}
func scripted(t *testing.T, rs ...provider.ChatResponse) provider.Model {
	t.Helper()
	m, e := provider.NewMockModel(provider.MockModelScript{Chat: rs})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func searchCall() provider.ChatResponse {
	return provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "logical-search-1", Name: "search_segments", Arguments: `{"query":"upload"}`}}}
}
func supported() provider.ChatResponse {
	return provider.ChatResponse{Content: `{"status":"answered","claims":[{"text":"上传后异步处理。","evidence_ids":["segment:s1"]}],"limitations":[]}`}
}
func TestEinoToolLoopPersistsEvidence(t *testing.T) {
	search := &testSearch{}
	snapshots := 0
	engine := Engine{Model: scripted(t, searchCall(), supported()), Search: search, Options: Options{MaxModelCalls: 4, MaxToolCalls: 3, AllowedTools: []string{"search_segments"}}, Hooks: Hooks{Save: func(context.Context, *State) error { snapshots++; return nil }}}
	state := &State{}
	result, e := engine.Run(context.Background(), "上传后呢？", nil, state)
	if e != nil {
		t.Fatal(e)
	}
	if len(result.Claims) != 1 || len(result.Evidence) != 1 || !result.Mock || state.ToolCalls != 1 || state.ModelCalls != 2 || search.calls != 1 || snapshots < 4 {
		t.Fatalf("bad loop state: result=%+v state=%+v snapshots=%d", result, state, snapshots)
	}
}
func TestCheckpointReusesCompletedToolAndFinal(t *testing.T) {
	search := &testSearch{}
	var checkpoint []byte
	first := Engine{Model: scripted(t, searchCall()), Search: search, Options: Options{MaxModelCalls: 4, MaxToolCalls: 3, AllowedTools: []string{"search_segments"}}, Hooks: Hooks{Save: func(_ context.Context, s *State) error { checkpoint, _ = json.Marshal(s); return nil }, Event: func(_ context.Context, kind string, _ any) error {
		if kind == "tool.completed" {
			return errors.New("simulated worker stop after persisted tool")
		}
		return nil
	}}}
	if _, e := first.Run(context.Background(), "fixture", nil, &State{}); e == nil {
		t.Fatal("expected simulated stop")
	}
	var state State
	if e := json.Unmarshal(checkpoint, &state); e != nil {
		t.Fatal(e)
	}
	if state.Pending == nil || len(state.ToolResults) != 1 {
		t.Fatalf("incomplete checkpoint: %+v", state)
	}
	resumed := Engine{Model: scripted(t, supported()), Search: search, Options: first.Options}
	result, e := resumed.Run(context.Background(), "fixture", nil, &state)
	if e != nil {
		t.Fatal(e)
	}
	if search.calls != 1 || state.ToolCalls != 1 || state.ModelCalls != 2 || len(result.Evidence) != 1 {
		t.Fatalf("tool replayed or budget reset: %+v", state)
	}
	resumed.Model = scripted(t, provider.ChatResponse{Content: "must not be consumed"})
	if _, e = resumed.Run(context.Background(), "fixture", nil, &state); e != nil {
		t.Fatal(e)
	}
	if state.ModelCalls != 2 {
		t.Fatal("completed checkpoint called model again")
	}
}
func TestModelBudgetSurvivesRestart(t *testing.T) {
	engine := Engine{Model: scripted(t, supported()), Search: &testSearch{}, Options: Options{MaxModelCalls: 2}}
	_, e := engine.Run(context.Background(), "fixture", nil, &State{ModelCalls: 2})
	if e == nil {
		t.Fatal("exhausted budget accepted")
	}
}
func TestAnswerRejectsTrailingJSON(t *testing.T) {
	_, e := Validate(`{"status":"insufficient_evidence","claims":[],"limitations":[]} {"extra":true}`, nil)
	if e == nil {
		t.Fatal("trailing JSON accepted")
	}
}
