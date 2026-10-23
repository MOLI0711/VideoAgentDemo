//go:build integration

package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
)

type meterFixtureModel struct {
	chat func(context.Context, provider.ChatRequest) (provider.ChatResponse, error)
}

func (m meterFixtureModel) Chat(ctx context.Context, req provider.ChatRequest) (provider.ChatResponse, error) {
	return m.chat(ctx, req)
}
func (meterFixtureModel) Embed(context.Context, []string) (provider.EmbeddingResponse, error) {
	return provider.EmbeddingResponse{}, errors.New("unexpected embedding request")
}
func (meterFixtureModel) Vision(context.Context, provider.VisionRequest) (provider.ChatResponse, error) {
	return provider.ChatResponse{}, errors.New("unexpected vision request")
}

func TestTextMeterMySQLSettlement(t *testing.T) {
	s := summaryTestStore(t)
	l := summaryFixture(t, s, 1)
	ctx := context.Background()
	worker := Worker{Store: s, ProviderRef: "fixture-provider", ChatModelID: "fixture-text"}
	latest := func(t *testing.T) domain.ModelCall {
		t.Helper()
		var call domain.ModelCall
		if err := s.DB.Get(&call, "SELECT * FROM model_calls WHERE job_id=? ORDER BY created_at DESC,id DESC LIMIT 1", l.Job.ID); err != nil {
			t.Fatal(err)
		}
		return call
	}
	request := provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: "fixture"}}, MaxTokens: 100, JSONMode: true}
	t.Run("invalid answer still settles known usage", func(t *testing.T) {
		worker.Model = meterFixtureModel{chat: func(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Content: "not valid answer JSON", Usage: provider.Usage{Known: true, TotalTokens: 37, Mock: true}, Mock: true}, nil
		}}
		if _, err := worker.summary(ctx, l); err == nil {
			t.Fatal("invalid answer accepted")
		}
		call := latest(t)
		if call.Status != "SETTLED" || !call.UsageKnown || call.ActualTokens != 37 || call.ProviderRef != "fixture-provider" || call.ModelID != "fixture-text" {
			t.Fatalf("answer validation erased paid known usage: %+v", call)
		}
	})
	t.Run("network uncertainty retains the complete reservation", func(t *testing.T) {
		worker.Model = meterFixtureModel{chat: func(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{}, errors.New("fixture network response lost")
		}}
		if _, err := worker.meter(l, 1500000).Chat(ctx, request); err == nil {
			t.Fatal("expected network failure")
		}
		call := latest(t)
		if call.Status != "UNKNOWN" || call.UsageKnown || call.ActualTokens != 0 || call.ReservedTokens <= request.MaxTokens {
			t.Fatalf("unknown usage released reservation: %+v", call)
		}
	})
	t.Run("invalid provider usage cannot release the reservation", func(t *testing.T) {
		worker.Model = meterFixtureModel{chat: func(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
			return provider.ChatResponse{Usage: provider.Usage{Known: true, TotalTokens: -1}}, nil
		}}
		if _, err := worker.meter(l, 1500000).Chat(ctx, request); err == nil {
			t.Fatal("invalid provider usage accepted")
		}
		call := latest(t)
		if call.Status != "UNKNOWN" || call.UsageKnown || call.ActualTokens != 0 || call.ReservedTokens == 0 {
			t.Fatalf("invalid usage released reservation: %+v", call)
		}
	})
	t.Run("a result after lease loss cannot settle usage", func(t *testing.T) {
		worker.Model = meterFixtureModel{chat: func(context.Context, provider.ChatRequest) (provider.ChatResponse, error) {
			if _, err := s.DB.Exec("UPDATE job_steps SET lease_until=TIMESTAMPADD(SECOND,-1,UTC_TIMESTAMP(6)) WHERE id=?", l.Step.ID); err != nil {
				t.Fatal(err)
			}
			return provider.ChatResponse{Content: "fixture", Usage: provider.Usage{Known: true, TotalTokens: 1}}, nil
		}}
		if _, err := worker.meter(l, 1500000).Chat(ctx, request); !errors.Is(err, domain.ErrLeaseLost) {
			t.Fatal("old worker settled a response", err)
		}
		call := latest(t)
		if call.Status != "RESERVED" || call.UsageKnown || call.ActualTokens != 0 || call.ReservedTokens == 0 {
			t.Fatalf("expired lease released reservation: %+v", call)
		}
	})
}
