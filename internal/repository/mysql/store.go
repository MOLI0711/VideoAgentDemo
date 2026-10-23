package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/MOLI0711/MOLIVedio/migrations"
	driver "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

type Store struct{ DB *sqlx.DB }

func New(db *sqlx.DB) *Store { return &Store{DB: db} }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.MultiStatements = false
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+00:00'"
	db, err := sqlx.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return New(db), nil
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	c, err := s.DB.Connx(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	var locked int
	if err = c.GetContext(ctx, &locked, "SELECT GET_LOCK('molivideo_migrations',30)"); err != nil {
		return err
	}
	if locked != 1 {
		return errors.New("migration lock unavailable")
	}
	defer c.ExecContext(context.Background(), "SELECT RELEASE_LOCK('molivideo_migrations')")
	if _, err = c.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations(version VARCHAR(100) PRIMARY KEY, checksum CHAR(64) NOT NULL, applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6))"); err != nil {
		return err
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		data, err := migrations.Files.ReadFile(e.Name())
		if err != nil {
			return err
		}
		sum := hash(data)
		var old string
		err = c.GetContext(ctx, &old, "SELECT checksum FROM schema_migrations WHERE version=?", e.Name())
		if err == nil {
			if old != sum {
				return fmt.Errorf("migration %s checksum changed", e.Name())
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		for _, statement := range strings.Split(string(data), ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err = c.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration %s: %w", e.Name(), err)
			}
		}
		if _, err = c.ExecContext(ctx, "INSERT INTO schema_migrations(version,checksum) VALUES(?,?)", e.Name(), sum); err != nil {
			return err
		}
	}
	return nil
}

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func js(b json.RawMessage) string {
	if len(b) == 0 {
		return "{}"
	}
	return string(b)
}
func array(b json.RawMessage) string {
	if len(b) == 0 {
		return "[]"
	}
	return string(b)
}
func mapped(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	var me *driver.MySQLError
	if errors.As(err, &me) && me.Number == 1062 {
		return domain.ErrConflict
	}
	return err
}
func bounded(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// Content timelines need a larger cap than ordinary resource lists. Executors
// that require complete content use the unpaginated Load* methods after scope
// validation, so a list limit can never silently truncate a whole-video result.
func boundedContent(n int) int {
	if n <= 0 {
		return 1000
	}
	if n > 10000 {
		return 10000
	}
	return n
}
func active(status string) bool { return status == domain.Queued || status == domain.Running }

func (s *Store) tx(ctx context.Context, fn func(*sqlx.Tx) error) error {
	t, err := s.DB.BeginTxx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer t.Rollback()
	if err = fn(t); err != nil {
		return mapped(err)
	}
	if err = t.Commit(); err != nil {
		return fmt.Errorf("%w: %w", domain.ErrCommitUnknown, err)
	}
	return nil
}

func quota(ctx context.Context, t *sqlx.Tx, owner domain.ID) error {
	// INSERT IGNORE can leave concurrent callers holding shared duplicate-key
	// locks, which deadlock when each then upgrades to FOR UPDATE. The no-op
	// update acquires an exclusive lock directly and serializes quota checks.
	if _, err := t.ExecContext(ctx, "INSERT INTO quota_guards(owner_id) VALUES(?) ON DUPLICATE KEY UPDATE owner_id=owner_id", owner); err != nil {
		return err
	}
	var id domain.ID
	return t.GetContext(ctx, &id, "SELECT owner_id FROM quota_guards WHERE owner_id=? FOR UPDATE", owner)
}

func idem(ctx context.Context, t *sqlx.Tx, owner domain.ID, scope, key string, body any) (domain.ID, error) {
	if key == "" {
		return domain.ID{}, nil
	}
	if len(key) > 128 {
		return domain.ID{}, domain.ErrInvalid
	}
	b, err := json.Marshal(body)
	if err != nil {
		return domain.ID{}, err
	}
	var row struct {
		ID   domain.ID `db:"resource_id"`
		Hash string    `db:"request_hash"`
	}
	err = t.GetContext(ctx, &row, "SELECT resource_id,request_hash FROM api_idempotency WHERE owner_id=? AND scope=? AND idempotency_key=?", owner, scope, key)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ID{}, nil
	}
	if err != nil {
		return domain.ID{}, err
	}
	if row.Hash != hash(b) {
		return domain.ID{}, domain.ErrConflict
	}
	return row.ID, nil
}

func saveIdem(ctx context.Context, t *sqlx.Tx, owner domain.ID, scope, key string, body any, id domain.ID) error {
	if key == "" {
		return nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = t.ExecContext(ctx, "INSERT INTO api_idempotency(owner_id,scope,idempotency_key,request_hash,resource_id) VALUES(?,?,?,?,?)", owner, scope, key, hash(b), id)
	return err
}

func (s *Store) SeedUser(ctx context.Context, name, passwordHash string) (*domain.User, error) {
	id := domain.NewID()
	_, err := s.DB.ExecContext(ctx, "INSERT INTO users(id,username,password_hash) VALUES(?,?,?) ON DUPLICATE KEY UPDATE username=username", id, name, passwordHash)
	if err != nil {
		return nil, err
	}
	return s.FindUser(ctx, name)
}
func (s *Store) FindUser(ctx context.Context, name string) (*domain.User, error) {
	var u domain.User
	err := s.DB.GetContext(ctx, &u, "SELECT * FROM users WHERE username=?", name)
	return &u, mapped(err)
}
func (s *Store) CreateAuthSession(ctx context.Context, user domain.ID, tokenHash string, ttl time.Duration) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO auth_sessions(token_hash,user_id,expires_at) VALUES(?,?,TIMESTAMPADD(SECOND,?,UTC_TIMESTAMP(6)))", tokenHash, user, int64(ttl.Seconds()))
	return mapped(err)
}
func (s *Store) Authenticate(ctx context.Context, tokenHash string) (*domain.User, error) {
	var u domain.User
	err := s.DB.GetContext(ctx, &u, "SELECT u.* FROM users u JOIN auth_sessions a ON a.user_id=u.id WHERE a.token_hash=? AND a.revoked_at IS NULL AND a.expires_at>UTC_TIMESTAMP(6)", tokenHash)
	return &u, mapped(err)
}
func (s *Store) RevokeAuthSession(ctx context.Context, tokenHash string) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE token_hash=?", tokenHash)
	return err
}
