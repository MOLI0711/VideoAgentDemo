// Package eventbus transfers durable dispatch events. Kafka consumers only
// accept work into MySQL; they never execute media or model calls.
package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/config"
	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/twmb/franz-go/pkg/kgo"
)

const consumerName = "video-dispatch"

type Store interface {
	ClaimOutbox(context.Context, int, time.Duration) ([]domain.OutboxEvent, error)
	MarkOutboxPublished(context.Context, domain.OutboxEvent) error
	RetryOutbox(context.Context, domain.OutboxEvent, time.Duration) error
	AcceptDispatch(context.Context, string, domain.DispatchEvent) error
	Quarantine(context.Context, string, int32, int64, string) error
}

type producer interface {
	ProduceSync(context.Context, ...*kgo.Record) kgo.ProduceResults
}

type committer interface {
	CommitRecords(context.Context, ...*kgo.Record) error
}

type Bus struct {
	store                  Store
	producer               producer
	client                 *kgo.Client
	brokers                []string
	mediaTopic, agentTopic string
	group                  string
	autoCreateTopics       bool
}

// New does not establish broker connectivity; startup readiness/doctor should
// verify the configured broker separately. One Bus owns one producer client.
func New(c config.Config, store Store) (*Bus, error) {
	if store == nil || len(c.Kafka.Brokers) == 0 || c.Kafka.MediaTopic == "" || c.Kafka.AgentTopic == "" || c.Kafka.MediaTopic == c.Kafka.AgentTopic {
		return nil, errors.New("Kafka requires a store, brokers and two distinct topics")
	}
	options := []kgo.Opt{
		kgo.SeedBrokers(c.Kafka.Brokers...),
		kgo.ClientID("moli-outbox-producer"),
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.RecordDeliveryTimeout(20 * time.Second),
		kgo.ProducerLinger(10 * time.Millisecond),
		kgo.ProducerBatchMaxBytes(128 << 10),
	}
	autoCreate := c.App.Env != "production"
	if autoCreate {
		options = append(options, kgo.AllowAutoTopicCreation())
	}
	client, err := kgo.NewClient(options...)
	if err != nil {
		return nil, err
	}
	return &Bus{store: store, producer: client, client: client, brokers: append([]string{}, c.Kafka.Brokers...), mediaTopic: c.Kafka.MediaTopic, agentTopic: c.Kafka.AgentTopic, group: consumerName, autoCreateTopics: autoCreate}, nil
}

func (b *Bus) groupName() string {
	if b.group != "" {
		return b.group
	}
	return consumerName
}

func (b *Bus) Close() {
	if b.client != nil {
		b.client.Close()
	}
}

func decodeDispatch(payload []byte) (domain.DispatchEvent, error) {
	var event domain.DispatchEvent
	if len(payload) > 64<<10 || json.Unmarshal(payload, &event) != nil || event.SchemaVersion != 1 || event.EventID == (domain.ID{}) || event.JobID == (domain.ID{}) || event.StepID == (domain.ID{}) || event.DispatchSeq == 0 || (event.WorkerKind != "media" && event.WorkerKind != "agent") {
		return event, domain.ErrInvalid
	}
	return event, nil
}

// PublishBatch claims in a short transaction, waits for Kafka outside that
// transaction, then conditionally marks each original publish attempt. Broker
// success followed by DB failure is safe to replay with the same event_id.
func (b *Bus) PublishBatch(ctx context.Context) (int, error) {
	ctx, deadline := context.WithTimeout(ctx, 45*time.Second)
	defer deadline()
	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	events, err := b.store.ClaimOutbox(claimCtx, 50, 60*time.Second)
	cancel()
	if err != nil {
		return 0, err
	}
	records := make([]*kgo.Record, 0, len(events))
	byRecord := make(map[*kgo.Record]domain.OutboxEvent, len(events))
	var failures []error
	for _, event := range events {
		dispatch, err := decodeDispatch(event.Payload)
		if err != nil || dispatch.EventID != event.EventID || dispatch.StepID != event.StepID || dispatch.DispatchSeq != event.DispatchSeq {
			retryCtx, stop := context.WithTimeout(ctx, 5*time.Second)
			retryErr := b.store.RetryOutbox(retryCtx, event, 5*time.Minute)
			stop()
			failures = append(failures, errors.Join(errors.New("invalid persisted dispatch event"), retryErr))
			continue
		}
		topic := b.mediaTopic
		if dispatch.WorkerKind == "agent" {
			topic = b.agentTopic
		}
		record := &kgo.Record{Topic: topic, Key: []byte(dispatch.JobID.String()), Value: append([]byte{}, event.Payload...)}
		records = append(records, record)
		byRecord[record] = event
	}
	if len(records) == 0 {
		return 0, errors.Join(failures...)
	}
	publishCtx, stop := context.WithTimeout(ctx, 25*time.Second)
	results := b.producer.ProduceSync(publishCtx, records...)
	stop()
	published := 0
	for _, result := range results {
		event := byRecord[result.Record]
		updateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if result.Err != nil {
			failures = append(failures, errors.Join(result.Err, b.store.RetryOutbox(updateCtx, event, retryDelay(event.PublishAttempt))))
		} else if err := b.store.MarkOutboxPublished(updateCtx, event); err != nil {
			// Do not clear a lease on uncertain marking. Recovery claims it after
			// expiry; an old publisher must not affect a newer publisher attempt.
			failures = append(failures, err)
		} else {
			published++
		}
		cancel()
	}
	return published, errors.Join(failures...)
}

func retryDelay(attempt uint64) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<attempt) * time.Second
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (b *Bus) RunProducer(ctx context.Context) error {
	for ctx.Err() == nil {
		count, err := b.PublishBatch(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			slog.Warn("outbox publish will retry", "error_type", fmt.Sprintf("%T", err))
		}
		if count == 0 || err != nil {
			if err := wait(ctx, time.Second); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

// acceptBatch never commits a later offset after an earlier DB failure. A
// partly accepted batch is replayed by closing/rejoining the consumer group;
// Inbox deduplication covers already-accepted records. Poison data only stores
// topic/partition/offset and a fixed error code, never the raw payload.
func (b *Bus) acceptBatch(ctx context.Context, records []*kgo.Record, client committer) error {
	for _, record := range records {
		event, err := decodeDispatch(record.Value)
		code := "INVALID_DISPATCH_SCHEMA"
		if err == nil && ((record.Topic == b.mediaTopic && event.WorkerKind != "media") || (record.Topic == b.agentTopic && event.WorkerKind != "agent") || (record.Topic != b.mediaTopic && record.Topic != b.agentTopic)) {
			err = domain.ErrInvalid
			code = "DISPATCH_TOPIC_MISMATCH"
		}
		if err != nil {
			if err = b.store.Quarantine(ctx, record.Topic, record.Partition, record.Offset, code); err != nil {
				return err
			}
			continue
		}
		if err = b.store.AcceptDispatch(ctx, b.groupName(), event); err != nil {
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, domain.ErrInvalid) {
				// Outbox publication follows the job transaction, so a missing
				// referenced job cannot become valid through ordinary delivery.
				if quarantineErr := b.store.Quarantine(ctx, record.Topic, record.Partition, record.Offset, "DISPATCH_RESOURCE_INVALID"); quarantineErr != nil {
					return quarantineErr
				}
				continue
			}
			return err
		}
	}
	if len(records) == 0 {
		return nil
	}
	return client.CommitRecords(ctx, records...)
}

func (b *Bus) consumeSession(ctx context.Context) error {
	options := []kgo.Opt{
		kgo.SeedBrokers(b.brokers...),
		kgo.ClientID("moli-dispatch-consumer"),
		kgo.ConsumerGroup(b.groupName()),
		kgo.ConsumeTopics(b.mediaTopic, b.agentTopic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.RebalanceTimeout(60 * time.Second),
		kgo.FetchMaxBytes(1 << 20),
		kgo.FetchMaxWait(500 * time.Millisecond),
	}
	if b.autoCreateTopics {
		options = append(options, kgo.AllowAutoTopicCreation())
	}
	client, err := kgo.NewClient(options...)
	if err != nil {
		return err
	}
	defer client.CloseAllowingRebalance()
	for ctx.Err() == nil {
		fetches := client.PollRecords(ctx, 50)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errs := fetches.Errors(); len(errs) != 0 {
			return errs[0].Err
		}
		records := fetches.Records()
		batchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := b.acceptBatch(batchCtx, records, client)
		cancel()
		client.AllowRebalance()
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (b *Bus) RunConsumer(ctx context.Context) error {
	for ctx.Err() == nil {
		err := b.consumeSession(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("dispatch consumer reconnecting", "error_type", fmt.Sprintf("%T", err))
		if err := wait(ctx, time.Second); err != nil {
			return err
		}
	}
	return ctx.Err()
}
