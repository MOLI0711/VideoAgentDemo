package mysql

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/jmoiron/sqlx"
)

// CreateGuestSession creates a private owner and its first usable assistant in
// one transaction. The caller owns the random bearer token and passes only its
// SHA256 digest. Existing valid browser sessions are reused by the HTTP layer.
func (s *Store) CreateGuestSession(ctx context.Context, tokenHash string, ttl time.Duration, defaultAgentConfig json.RawMessage) (*domain.User, error) {
	tokenHash = strings.ToLower(tokenHash)
	_, hashErr := hex.DecodeString(tokenHash)
	var config map[string]json.RawMessage
	if len(tokenHash) != 64 || hashErr != nil || ttl < time.Second || len(defaultAgentConfig) > 64<<10 || json.Unmarshal(defaultAgentConfig, &config) != nil || config == nil {
		return nil, domain.ErrInvalid
	}
	var out domain.User
	err := s.tx(ctx, func(t *sqlx.Tx) error {
		userID, agentID, versionID := domain.NewID(), domain.NewID(), domain.NewID()
		name := "访客-" + strings.ReplaceAll(userID.String(), "-", "")[:16]
		// This is deliberately not a password hash in any supported format.
		// A guest identity can only be recovered using its valid browser session.
		if _, err := t.ExecContext(ctx, "INSERT INTO users(id,username,password_hash,is_guest) VALUES(?,?,'!guest-session-only',TRUE)", userID, name); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO auth_sessions(token_hash,user_id,expires_at) VALUES(?,?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)))", tokenHash, userID, int64(ttl/time.Second)); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO agents(id,owner_id,name,description,status,active_version_id) VALUES(?,?,'视频助手','根据视频内容回答问题并提供可追溯的证据。','ACTIVE',?)", agentID, userID, versionID); err != nil {
			return err
		}
		if _, err := t.ExecContext(ctx, "INSERT INTO agent_versions(id,agent_id,version_no,status,config) VALUES(?,?,1,'PUBLISHED',?)", versionID, agentID, defaultAgentConfig); err != nil {
			return err
		}
		return t.GetContext(ctx, &out, "SELECT * FROM users WHERE id=?", userID)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
