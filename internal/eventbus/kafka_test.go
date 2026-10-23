package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"github.com/twmb/franz-go/pkg/kgo"
)

type quarantineEntry struct {
	topic     string
	partition int32
	offset    int64
	code      string
}
type fakeStore struct {
	outbox                            []domain.OutboxEvent
	marked, retried                   []domain.OutboxEvent
	accepted                          []domain.DispatchEvent
	quarantined                       []quarantineEntry
	acceptErr, markErr, quarantineErr error
	failAcceptAt                      int
	order                             *[]string
}

func (s *fakeStore) ClaimOutbox(context.Context, int, time.Duration) ([]domain.OutboxEvent, error) {
	return s.outbox, nil
}
func (s *fakeStore) MarkOutboxPublished(_ context.Context, event domain.OutboxEvent) error {
	if s.markErr != nil {
		return s.markErr
	}
	s.marked = append(s.marked, event)
	return nil
}
func (s *fakeStore) RetryOutbox(_ context.Context, event domain.OutboxEvent, _ time.Duration) error {
	s.retried = append(s.retried, event)
	return nil
}
func (s *fakeStore) AcceptDispatch(_ context.Context, _ string, event domain.DispatchEvent) error {
	if s.acceptErr != nil && (s.failAcceptAt == 0 || len(s.accepted)+1 == s.failAcceptAt) {
		return s.acceptErr
	}
	s.accepted = append(s.accepted, event)
	if s.order != nil {
		*s.order = append(*s.order, "db")
	}
	return nil
}
func (s *fakeStore) Quarantine(_ context.Context, topic string, partition int32, offset int64, code string) error {
	if s.quarantineErr != nil {
		return s.quarantineErr
	}
	s.quarantined = append(s.quarantined, quarantineEntry{topic, partition, offset, code})
	return nil
}

type fakeProducer struct {
	err     error
	seen    []*kgo.Record
	reverse bool
}

func (p *fakeProducer) ProduceSync(_ context.Context, records ...*kgo.Record) kgo.ProduceResults {
	p.seen = append(p.seen, records...)
	out := make(kgo.ProduceResults, 0, len(records))
	for _, record := range records {
		out = append(out, kgo.ProduceResult{Record: record, Err: p.err})
	}
	if p.reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

type fakeCommitter struct {
	committed []*kgo.Record
	err       error
	order     *[]string
}

func (c *fakeCommitter) CommitRecords(_ context.Context, records ...*kgo.Record) error {
	if c.order != nil {
		*c.order = append(*c.order, "offset")
	}
	if c.err != nil {
		return c.err
	}
	c.committed = append(c.committed, records...)
	return nil
}

func dispatch(kind string) (domain.DispatchEvent, domain.OutboxEvent) {
	event := domain.DispatchEvent{SchemaVersion: 1, EventID: domain.NewID(), JobID: domain.NewID(), StepID: domain.NewID(), DispatchSeq: 3, WorkerKind: kind}
	payload, _ := json.Marshal(event)
	return event, domain.OutboxEvent{EventID: event.EventID, StepID: event.StepID, DispatchSeq: event.DispatchSeq, Payload: payload, PublishAttempt: 2}
}
func record(kind string, offset int64) *kgo.Record {
	_, event := dispatch(kind)
	return &kgo.Record{Topic: "video." + kind, Partition: 0, Offset: offset, Value: event.Payload}
}
func bus(store *fakeStore, producer *fakeProducer) *Bus {
	return &Bus{store: store, producer: producer, mediaTopic: "video.media", agentTopic: "video.agent"}
}

func TestPublishOnlyMarksAfterBrokerAck(t *testing.T) {
	_, event := dispatch("media")
	store := &fakeStore{outbox: []domain.OutboxEvent{event}}
	producer := &fakeProducer{err: errors.New("broker unavailable")}
	n, err := bus(store, producer).PublishBatch(context.Background())
	if err == nil || n != 0 || len(store.marked) != 0 || len(store.retried) != 1 {
		t.Fatalf("failed publish marked as complete: n=%d err=%v", n, err)
	}
	producer.err = nil
	n, err = bus(store, producer).PublishBatch(context.Background())
	if err != nil || n != 1 || len(store.marked) != 1 {
		t.Fatalf("successful retry not marked: %d %v", n, err)
	}
	if string(producer.seen[0].Value) != string(producer.seen[1].Value) {
		t.Fatal("retry changed stable event payload")
	}
}

func TestPublishAcknowledgementsCanArriveOutOfOrder(t *testing.T) {
	first, a := dispatch("media")
	second, b := dispatch("agent")
	store := &fakeStore{outbox: []domain.OutboxEvent{a, b}}
	producer := &fakeProducer{reverse: true}
	n, err := bus(store, producer).PublishBatch(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("publish: %d %v", n, err)
	}
	if store.marked[0].EventID != b.EventID || store.marked[1].EventID != a.EventID {
		t.Fatal("result mapped to wrong publish attempt")
	}
	if producer.seen[0].Topic != "video.media" || producer.seen[1].Topic != "video.agent" {
		t.Fatal("incorrect worker topic")
	}
	if string(producer.seen[0].Key) != first.JobID.String() || string(producer.seen[1].Key) != second.JobID.String() {
		t.Fatal("partition key must group job dispatch events")
	}
}

func TestAckThenLostPublishLeaseLeavesRecoveryToDatabase(t *testing.T) {
	_, event := dispatch("media")
	store := &fakeStore{outbox: []domain.OutboxEvent{event}, markErr: domain.ErrLeaseLost}
	n, err := bus(store, &fakeProducer{}).PublishBatch(context.Background())
	if !errors.Is(err, domain.ErrLeaseLost) || n != 0 || len(store.retried) != 0 {
		t.Fatal("old publisher must not reset a newer lease")
	}
}

func TestInvalidOutboxDoesNotReachKafka(t *testing.T) {
	_, event := dispatch("media")
	event.EventID = domain.NewID()
	store := &fakeStore{outbox: []domain.OutboxEvent{event}}
	producer := &fakeProducer{}
	_, err := bus(store, producer).PublishBatch(context.Background())
	if err == nil || len(producer.seen) != 0 || len(store.retried) != 1 {
		t.Fatal("invalid stored payload published")
	}
}

func TestInboxCommitsBeforeKafkaOffset(t *testing.T) {
	order := []string{}
	store := &fakeStore{order: &order}
	committer := &fakeCommitter{order: &order}
	input := []*kgo.Record{record("media", 7), record("media", 8), record("agent", 3)}
	if err := bus(store, nil).acceptBatch(context.Background(), input, committer); err != nil {
		t.Fatal(err)
	}
	if len(order) != 4 || order[0] != "db" || order[1] != "db" || order[2] != "db" || order[3] != "offset" {
		t.Fatalf("wrong commit order: %v", order)
	}
	if len(committer.committed) != 3 {
		t.Fatal("offsets missing")
	}
}

func TestDatabaseFailureNeverCommitsBeyondUnacceptedRecord(t *testing.T) {
	store := &fakeStore{acceptErr: errors.New("database timeout"), failAcceptAt: 2}
	committer := &fakeCommitter{}
	input := []*kgo.Record{record("media", 7), record("media", 8), record("media", 9)}
	err := bus(store, nil).acceptBatch(context.Background(), input, committer)
	if err == nil || len(store.accepted) != 1 || len(committer.committed) != 0 {
		t.Fatal("unaccepted offset was committed")
	}
}

func TestPoisonEventRequiresDurableQuarantineBeforeCommit(t *testing.T) {
	store := &fakeStore{}
	committer := &fakeCommitter{}
	input := []*kgo.Record{{Topic: "video.media", Partition: 2, Offset: 11, Value: []byte(`secret untrusted payload`)}}
	if err := bus(store, nil).acceptBatch(context.Background(), input, committer); err != nil {
		t.Fatal(err)
	}
	if len(store.accepted) != 0 || len(store.quarantined) != 1 || len(committer.committed) != 1 {
		t.Fatal("poison event was not quarantined before offset commit")
	}
	entry := store.quarantined[0]
	if entry.topic != "video.media" || entry.partition != 2 || entry.offset != 11 || entry.code != "INVALID_DISPATCH_SCHEMA" {
		t.Fatalf("bad quarantine metadata: %#v", entry)
	}
	store.quarantineErr = errors.New("quarantine write failed")
	committer.committed = nil
	if err := bus(store, nil).acceptBatch(context.Background(), input, committer); err == nil || len(committer.committed) != 0 {
		t.Fatal("failed quarantine must be replayed")
	}
}

func TestWrongWorkerTopicIsQuarantined(t *testing.T) {
	store := &fakeStore{}
	committer := &fakeCommitter{}
	wrong := record("agent", 3)
	wrong.Topic = "video.media"
	if err := bus(store, nil).acceptBatch(context.Background(), []*kgo.Record{wrong}, committer); err != nil {
		t.Fatal(err)
	}
	if len(store.accepted) != 0 || len(store.quarantined) != 1 || store.quarantined[0].code != "DISPATCH_TOPIC_MISMATCH" {
		t.Fatal("cross-topic dispatch reached Inbox")
	}
}

func TestCommitFailureIsReturnedForReplay(t *testing.T) {
	store := &fakeStore{}
	committer := &fakeCommitter{err: errors.New("offset response lost")}
	if err := bus(store, nil).acceptBatch(context.Background(), []*kgo.Record{record("media", 7)}, committer); err == nil || len(store.accepted) != 1 {
		t.Fatal("commit failure was swallowed")
	}
}

func TestDispatchValidation(t *testing.T) {
	event, _ := dispatch("media")
	for _, change := range []func(*domain.DispatchEvent){func(e *domain.DispatchEvent) { e.SchemaVersion = 2 }, func(e *domain.DispatchEvent) { e.JobID = domain.ID{} }, func(e *domain.DispatchEvent) { e.DispatchSeq = 0 }, func(e *domain.DispatchEvent) { e.WorkerKind = "unknown" }} {
		invalid := event
		change(&invalid)
		payload, _ := json.Marshal(invalid)
		if _, err := decodeDispatch(payload); !errors.Is(err, domain.ErrInvalid) {
			t.Fatal("invalid dispatch schema accepted")
		}
	}
	if retryDelay(100) > time.Minute+4*time.Second {
		t.Fatal("retry delay must stay bounded")
	}
}
