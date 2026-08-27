package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
	"github.com/EshkirenokCode/llm-gateway/internal/billing"
	"github.com/EshkirenokCode/llm-gateway/internal/storage/db"
)

// IdentityByKeyHash implements auth.Lookup.
func (s *Store) IdentityByKeyHash(ctx context.Context, hash []byte) (auth.Identity, error) {
	row, err := s.GetAPIKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Identity{}, auth.ErrUnknownKey
		}
		return auth.Identity{}, fmt.Errorf("look up api key: %w", err)
	}

	if row.RevokedAt.Valid {
		return auth.Identity{}, auth.ErrRevoked
	}
	if row.TenantDisabledAt.Valid {
		return auth.Identity{}, auth.ErrTenantDisabled
	}

	identity := auth.Identity{
		TenantID:       row.TenantID,
		TenantName:     row.TenantName,
		TenantPrefix:   row.TenantPrefix,
		KeyID:          row.KeyID,
		SoftLimitRatio: numericToFloat(row.SoftLimitRatio, 0.8),
	}

	if row.RateLimitRpm != nil {
		identity.RateLimitRPM = int(*row.RateLimitRpm)
	}
	if row.BudgetMicros != nil {
		identity.BudgetMicros = *row.BudgetMicros
		identity.HasBudget = true
	}

	return identity, nil
}

// MarkKeyUsed implements auth.Lookup. It runs in the background: the timestamp
// is useful for auditing but must never slow down or fail a request.
func (s *Store) MarkKeyUsed(_ context.Context, keyID uuid.UUID) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.TouchAPIKey(ctx, keyID)
	}()
}

// SpentSince implements billing.SpendReader.
func (s *Store) SpentSince(ctx context.Context, tenantID uuid.UUID, since time.Time) (billing.Micros, error) {
	total, err := s.SumTenantSpend(ctx, db.SumTenantSpendParams{
		TenantID:  tenantID,
		CreatedAt: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return 0, fmt.Errorf("sum tenant spend: %w", err)
	}
	return billing.Micros(total), nil
}

// RequestRecord is one finished request, as written to the requests table.
type RequestRecord struct {
	TenantID         uuid.UUID
	APIKeyID         uuid.UUID
	Provider         string
	Model            string
	Streamed         bool
	StatusCode       int
	PromptTokens     int
	CompletionTokens int
	Cost             billing.Micros
	Duration         time.Duration
	Err              string
}

// RecordRequest persists a finished request and returns its id.
func (s *Store) RecordRequest(ctx context.Context, rec RequestRecord) (uuid.UUID, error) {
	params := db.InsertRequestParams{
		TenantID:         rec.TenantID,
		Provider:         rec.Provider,
		Model:            rec.Model,
		Streamed:         rec.Streamed,
		StatusCode:       int32(rec.StatusCode),
		PromptTokens:     int32(rec.PromptTokens),
		CompletionTokens: int32(rec.CompletionTokens),
		CostMicros:       int64(rec.Cost),
		DurationMs:       int32(rec.Duration.Milliseconds()),
	}

	if rec.APIKeyID != uuid.Nil {
		id := rec.APIKeyID
		params.ApiKeyID = &id
	}
	if rec.Err != "" {
		msg := rec.Err
		params.Error = &msg
	}

	row, err := s.InsertRequest(ctx, params)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert request: %w", err)
	}

	return row.ID, nil
}

// StoredResponse is a response replayed for a repeated Idempotency-Key.
type StoredResponse struct {
	StatusCode  int
	Body        []byte
	RequestHash []byte
}

// FindIdempotent returns a previously stored response, if any.
func (s *Store) FindIdempotent(ctx context.Context, tenantID uuid.UUID, key string) (*StoredResponse, error) {
	row, err := s.GetIdempotentResponse(ctx, db.GetIdempotentResponseParams{TenantID: tenantID, Key: key})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read idempotency key: %w", err)
	}

	return &StoredResponse{
		StatusCode:  int(row.StatusCode),
		Body:        row.ResponseBody,
		RequestHash: row.RequestHash,
	}, nil
}

// SaveIdempotent stores a response for replay. Conflicts are ignored: if two
// identical requests race, whichever lands first wins and the other simply
// returns its own equivalent answer.
func (s *Store) SaveIdempotent(ctx context.Context, tenantID uuid.UUID, key string, requestHash []byte, status int, body []byte, requestID uuid.UUID, ttl time.Duration) error {
	params := db.SaveIdempotentResponseParams{
		TenantID:     tenantID,
		Key:          key,
		RequestHash:  requestHash,
		StatusCode:   int32(status),
		ResponseBody: body,
		ExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(ttl), Valid: true},
	}
	if requestID != uuid.Nil {
		id := requestID
		params.RequestID = &id
	}

	if err := s.SaveIdempotentResponse(ctx, params); err != nil {
		return fmt.Errorf("save idempotency key: %w", err)
	}

	return nil
}

// Audit appends an entry to the audit log.
func (s *Store) Audit(ctx context.Context, tenantID, keyID uuid.UUID, action string, detail map[string]any) error {
	payload := []byte("{}")
	if len(detail) > 0 {
		encoded, err := json.Marshal(detail)
		if err != nil {
			return fmt.Errorf("encode audit detail: %w", err)
		}
		payload = encoded
	}

	params := db.InsertAuditEventParams{Action: action, Detail: payload}
	if tenantID != uuid.Nil {
		id := tenantID
		params.TenantID = &id
	}
	if keyID != uuid.Nil {
		id := keyID
		params.ApiKeyID = &id
	}

	if err := s.InsertAuditEvent(ctx, params); err != nil {
		return fmt.Errorf("insert audit event: %w", err)
	}

	return nil
}

func numericToFloat(n pgtype.Numeric, fallback float64) float64 {
	if !n.Valid || n.Int == nil {
		return fallback
	}

	f := new(big.Float).SetInt(n.Int)
	if n.Exp != 0 {
		f.Mul(f, big.NewFloat(pow10(n.Exp)))
	}

	value, _ := f.Float64()
	if value <= 0 || value > 1 {
		return fallback
	}

	return value
}

func pow10(exp int32) float64 {
	result := 1.0
	if exp > 0 {
		for i := int32(0); i < exp; i++ {
			result *= 10
		}
		return result
	}
	for i := int32(0); i > exp; i-- {
		result /= 10
	}
	return result
}
