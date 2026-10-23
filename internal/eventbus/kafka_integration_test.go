//go:build integration

package eventbus

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/config"
	"github.com/MOLI0711/MOLIVedio/internal/domain"
	db "github.com/MOLI0711/MOLIVedio/internal/repository/mysql"
	driver "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// This opt-in test uses real Kafka and MySQL, but no object storage or models.
// It creates and removes only randomly named test databases/topics/groups.
func TestKafkaMySQLDispatchIntegration(t *testing.T) {
	brokers := os.Getenv("VA_KAFKA_TEST_BROKERS")
	dsn := os.Getenv("VA_MYSQL_TEST_ADMIN_DSN")
	if brokers == "" || dsn == "" {
		t.Skip("set VA_KAFKA_TEST_BROKERS and VA_MYSQL_TEST_ADMIN_DSN for real Kafka/MySQL integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	parsed, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test admin DSN")
	}
	parsed.DBName = ""
	parsed.ParseTime = true
	adminDB, err := sqlx.ConnectContext(ctx, "mysql", parsed.FormatDSN())
	if err != nil {
		t.Fatal("test MySQL is unavailable")
	}
	suffix := strings.ReplaceAll(domain.NewID().String(), "-", "")
	name := "molivideo_kafka_test_" + suffix
	if _, err = adminDB.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"); err != nil {
		adminDB.Close()
		t.Fatal(err)
	}
	parsed.DBName = name
	store, err := db.Open(ctx, parsed.FormatDSN())
	if err != nil {
		adminDB.Exec("DROP DATABASE `" + name + "`")
		adminDB.Close()
		t.Fatal("open isolated test database")
	}
	t.Cleanup(func() {
		store.Close()
		if _, err := adminDB.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("remove own test database: %v", err)
		}
		adminDB.Close()
	})
	if err = store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	admin, err := kgo.NewClient(kgo.SeedBrokers(strings.Split(brokers, ",")...))
	if err != nil {
		t.Fatal(err)
	}
	mediaTopic, agentTopic := "moli-integration-"+suffix+"-media", "moli-integration-"+suffix+"-agent"
	// Leave both topics absent: a fresh local broker must create them through
	// the application clients, without a manual provisioning step.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		remove := kmsg.NewPtrDeleteTopicsRequest()
		remove.TopicNames = []string{mediaTopic, agentTopic}
		remove.Topics = []kmsg.DeleteTopicsRequestTopic{{Topic: &mediaTopic}, {Topic: &agentTopic}}
		if _, err := remove.RequestWith(cleanup, admin); err != nil {
			t.Errorf("remove own test topics: %v", err)
		}
		admin.Close()
	})
	var conf config.Config
	conf.App.Env = "development"
	conf.Kafka.Brokers = strings.Split(brokers, ",")
	conf.Kafka.MediaTopic = mediaTopic
	conf.Kafka.AgentTopic = agentTopic
	bus, err := New(conf, store)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	bus.group = "moli-integration-" + suffix
	consumerCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- bus.RunConsumer(consumerCtx) }()
	defer func() {
		stop()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("consumer did not stop")
		}
	}()
	user, err := store.SeedUser(ctx, "kafka-fixture", "not-a-login-password-hash")
	if err != nil {
		t.Fatal(err)
	}
	upload, err := store.CreateUpload(ctx, user.ID, domain.CreateUpload{Title: "Kafka metadata fixture", Filename: "synthetic.mp4", ContentType: "video/mp4", Size: 3, MultipartID: "fixture-no-s3", ObjectKey: "fixture/no-media", IdempotencyKey: domain.NewID().String()})
	if err != nil {
		t.Fatal(err)
	}
	job, err := store.CompleteUpload(ctx, user.ID, upload.ID, domain.Asset{ObjectKey: upload.ObjectKey, Size: 3, ContentType: "video/mp4", StorageBackend: "fixture"}, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var outbox domain.OutboxEvent
	if err = store.DB.GetContext(ctx, &outbox, "SELECT * FROM outbox_events LIMIT 1"); err != nil {
		t.Fatal(err)
	}
	count, err := bus.PublishBatch(ctx)
	if err != nil || count != 1 {
		t.Fatalf("publish: count=%d error=%v", count, err)
	}
	awaitCondition(t, ctx, func() bool {
		var state string
		return store.DB.GetContext(ctx, &state, "SELECT status FROM job_steps WHERE id=?", outbox.StepID) == nil && state == domain.Ready
	})
	var state string
	if err = store.DB.GetContext(ctx, &state, "SELECT status FROM outbox_events WHERE event_id=?", outbox.EventID); err != nil || state != "PUBLISHED" {
		t.Fatalf("outbox was not marked: %s %v", state, err)
	}
	// Duplicate delivery and a subsequent poison message use the same key, so
	// observing quarantine proves the duplicate passed through that partition.
	result := admin.ProduceSync(ctx,
		&kgo.Record{Topic: mediaTopic, Key: []byte(job.ID.String()), Value: outbox.Payload},
		&kgo.Record{Topic: mediaTopic, Key: []byte(job.ID.String()), Value: []byte(`{"schema_version":999}`)},
	)
	if err = result.FirstErr(); err != nil {
		t.Fatal(err)
	}
	awaitCondition(t, ctx, func() bool {
		var n int
		return store.DB.GetContext(ctx, &n, "SELECT COUNT(*) FROM quarantined_events WHERE topic=?", mediaTopic) == nil && n == 1
	})
	var inboxCount int
	if err = store.DB.GetContext(ctx, &inboxCount, "SELECT COUNT(*) FROM inbox_events"); err != nil || inboxCount != 1 {
		t.Fatalf("duplicate created another acceptance: count=%d error=%v", inboxCount, err)
	}
	var code string
	if err = store.DB.GetContext(ctx, &code, "SELECT error_code FROM quarantined_events LIMIT 1"); err != nil || code != "INVALID_DISPATCH_SCHEMA" {
		t.Fatalf("quarantine code: %s %v", code, err)
	}
	if err = store.DB.GetContext(ctx, &state, "SELECT status FROM job_steps WHERE id=?", outbox.StepID); err != nil || state != domain.Ready {
		t.Fatalf("duplicate changed READY step: %s %v", state, err)
	}
	t.Log("real Kafka broker + MySQL: fresh topics auto-created, Outbox published, Inbox accepted, duplicate deduplicated, poison quarantined")
}

func awaitCondition(t *testing.T, ctx context.Context, ready func() bool) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for real dispatch state")
			return
		case <-ticker.C:
		}
	}
}
