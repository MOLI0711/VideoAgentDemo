//go:build integration

package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"golang.org/x/crypto/bcrypt"
)

func TestMySQLGuestSessions(t *testing.T) {
	s := isolatedStore(t)
	ctx := context.Background()
	config := json.RawMessage(`{"model_ref":"default","tools":["search_segments"]}`)
	t.Run("private guest owns one published assistant and has no password", func(t *testing.T) {
		legacy, err := s.SeedUser(ctx, "legacy-user", "existing-test-only-hash")
		if err != nil || legacy.IsGuest {
			t.Fatal("legacy account changed", err)
		}
		tokenHash := hash([]byte(domain.NewID().String()))
		guest, err := s.CreateGuestSession(ctx, tokenHash, 30*24*time.Hour, config)
		if err != nil || !guest.IsGuest || guest.ID == legacy.ID || !strings.HasPrefix(guest.Username, "访客-") {
			t.Fatal("invalid guest", err)
		}
		if bcrypt.CompareHashAndPassword([]byte(guest.PasswordHash), []byte("!guest-session-only")) == nil {
			t.Fatal("guest sentinel is a usable password")
		}
		auth, err := s.Authenticate(ctx, tokenHash)
		if err != nil || auth.ID != guest.ID || !auth.IsGuest {
			t.Fatal("guest session unavailable after creation", err)
		}
		agents, err := s.ListAgents(ctx, guest.ID)
		if err != nil || len(agents) != 1 || agents[0].Name != "视频助手" || agents[0].Status != "ACTIVE" {
			t.Fatalf("default agent unavailable: %+v %v", agents, err)
		}
		versions, err := s.ListAgentVersions(ctx, guest.ID, agents[0].ID)
		if err != nil || len(versions) != 1 || versions[0].Status != "PUBLISHED" || versions[0].ID != agents[0].ActiveVersionID || canonical(versions[0].Config) != canonical(config) {
			t.Fatalf("default published version missing: %+v %v", versions, err)
		}
		videos, err := s.ListVideos(ctx, guest.ID, 50)
		if err != nil || len(videos) != 0 {
			t.Fatal("guest received another owner's or demo videos", err)
		}
		if _, err := s.GetAgent(ctx, legacy.ID, agents[0].ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("guest assistant readable by another owner", err)
		}
		storedLegacy, err := s.FindUser(ctx, legacy.Username)
		if err != nil || storedLegacy.PasswordHash != legacy.PasswordHash || storedLegacy.IsGuest {
			t.Fatal("guest creation changed legacy account", err)
		}
		before := map[string]int{}
		for _, table := range []string{"users", "auth_sessions", "agents", "agent_versions"} {
			var n int
			if err := s.DB.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil {
				t.Fatal(err)
			}
			before[table] = n
		}
		if _, err := s.CreateGuestSession(ctx, tokenHash, time.Hour, config); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("duplicate session token did not conflict", err)
		}
		for table, count := range before {
			var n int
			if err := s.DB.Get(&n, "SELECT COUNT(*) FROM "+table); err != nil || n != count {
				t.Fatalf("failed transaction leaked %s rows: before=%d after=%d error=%v", table, count, n, err)
			}
		}
		if err := s.RevokeAuthSession(ctx, tokenHash); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Authenticate(ctx, tokenHash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("revoked guest session accepted", err)
		}
	})
	t.Run("parallel guests never share an owner", func(t *testing.T) {
		type outcome struct {
			user *domain.User
			err  error
		}
		results := make(chan outcome, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				u, err := s.CreateGuestSession(ctx, hash([]byte(domain.NewID().String())), time.Hour, config)
				results <- outcome{u, err}
			}()
		}
		wg.Wait()
		close(results)
		owners := map[domain.ID]bool{}
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			if owners[result.user.ID] {
				t.Fatal("visitors shared an owner")
			}
			owners[result.user.ID] = true
			agents, err := s.ListAgents(ctx, result.user.ID)
			if err != nil || len(agents) != 1 || agents[0].OwnerID != result.user.ID {
				t.Fatal("visitor assistant scope mismatch", err)
			}
		}
	})
	t.Run("invalid initialization cannot create partial identity", func(t *testing.T) {
		for _, invalid := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`"config"`)} {
			if _, err := s.CreateGuestSession(ctx, hash([]byte(domain.NewID().String())), time.Hour, invalid); !errors.Is(err, domain.ErrInvalid) {
				t.Fatal("invalid config accepted", err)
			}
		}
		if _, err := s.CreateGuestSession(ctx, "not-a-session-hash", time.Hour, config); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal("invalid token hash accepted", err)
		}
		if _, err := s.CreateGuestSession(ctx, hash([]byte(domain.NewID().String())), 0, config); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal("invalid TTL accepted", err)
		}
	})
}
