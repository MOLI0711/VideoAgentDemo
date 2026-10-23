package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/internal/provider"
	db "github.com/MOLI0711/MOLIVedio/internal/repository/mysql"
)

// Request bytes + maximum output is a deliberately conservative text quota
// estimate, not an exact vendor tokenizer or billed image/embedding token count.
type meteredModel struct {
	base                 provider.Model
	store                *db.Store
	lease                domain.Lease
	limit                int
	providerRef, modelID string
}

func (m *meteredModel) Chat(ctx context.Context, request provider.ChatRequest) (provider.ChatResponse, error) {
	body, e := json.Marshal(request)
	if e != nil {
		return provider.ChatResponse{}, e
	}
	if len(body) > 200000 || request.MaxTokens <= 0 {
		return provider.ChatResponse{}, domain.ErrInvalid
	}
	hash := sha256.Sum256(body)
	providerRef, modelID := m.providerRef, m.modelID
	if providerRef == "" {
		providerRef = "configured-model"
	}
	if modelID == "" {
		modelID = "configured-chat"
	}
	reservation := domain.ModelReservation{CallKey: domain.NewID().String(), RequestHash: hex.EncodeToString(hash[:]), ProviderRef: providerRef, ModelID: modelID, ReservedTokens: len(body) + request.MaxTokens, MaxTokens: m.limit}
	call, created, e := m.store.ReserveModelCall(ctx, m.lease, reservation)
	if e != nil {
		return provider.ChatResponse{}, e
	}
	if !created {
		return provider.ChatResponse{}, domain.ErrConflict
	}
	response, requestErr := m.base.Chat(ctx, request)
	known := requestErr == nil && response.Usage.Known
	actual := 0
	if known {
		if response.Usage.TotalTokens < 0 || response.Usage.TotalTokens > 2147483647 {
			known = false
			requestErr = errors.New("invalid provider usage")
		} else {
			actual = int(response.Usage.TotalTokens)
		}
	}
	// Unknown outcomes retain the reservation; loss of the lease also prevents an
	// old worker from releasing a reservation after a new worker has taken over.
	settleErr := m.store.FinishModelCall(ctx, m.lease, call.ID, known, actual)
	if requestErr != nil || settleErr != nil {
		return provider.ChatResponse{}, errors.Join(requestErr, settleErr)
	}
	return response, nil
}
func (m *meteredModel) Embed(ctx context.Context, input []string) (provider.EmbeddingResponse, error) {
	return m.base.Embed(ctx, input)
}
func (m *meteredModel) Vision(ctx context.Context, input provider.VisionRequest) (provider.ChatResponse, error) {
	return m.base.Vision(ctx, input)
}
func (w *Worker) meter(l *domain.Lease, limit int) provider.Model {
	return &meteredModel{base: w.Model, store: w.Store, lease: *l, limit: limit, providerRef: w.ProviderRef, modelID: w.ChatModelID}
}
