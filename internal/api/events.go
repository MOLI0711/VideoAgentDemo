package api

import (
	"encoding/json"
	"fmt"
	"github.com/MOLI0711/MOLIVedio/internal/domain"
	"net/http"
	"strconv"
	"time"
)

func eventCursor(r *http.Request) (int64, error) {
	s := r.Header.Get("Last-Event-ID")
	if s == "" {
		s = r.URL.Query().Get("after_seq")
	}
	if s == "" {
		return 0, nil
	}
	n, e := strconv.ParseInt(s, 10, 64)
	if e != nil || n < 0 {
		return 0, domain.ErrInvalid
	}
	return n, nil
}
func (a *Server) events(w http.ResponseWriter, r *http.Request) error {
	i, e := id(r, "id")
	if e != nil {
		return e
	}
	cursor, e := eventCursor(r)
	if e != nil {
		return e
	}
	snapshot, e := a.Store.GetRun(r.Context(), user(r).ID, i)
	if e != nil {
		return e
	}
	if cursor > snapshot.NextEventSeq {
		return domain.ErrInvalid
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	if e = rc.Flush(); e != nil {
		return nil
	}
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		events, e := a.Store.ListRunEvents(r.Context(), user(r).ID, i, cursor, 100)
		if e != nil {
			return nil
		}
		for _, event := range events {
			payload := map[string]any{}
			_ = json.Unmarshal(event.Payload, &payload)
			payload["attempt_no"] = event.AttemptNo
			b, _ := json.Marshal(payload)
			if _, e = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, b); e != nil {
				return nil
			}
			cursor = event.Seq
		}
		if len(events) > 0 {
			if e = rc.Flush(); e != nil {
				return nil
			}
		}
		snapshot, e = a.Store.GetRun(r.Context(), user(r).ID, i)
		if e != nil {
			return nil
		}
		if domain.Terminal(snapshot.Status) && cursor >= snapshot.NextEventSeq {
			return nil
		}
		if len(events) == 100 {
			continue
		}
		select {
		case <-r.Context().Done():
			return nil
		case <-poll.C:
		case <-heartbeat.C:
			if _, e = fmt.Fprint(w, ": heartbeat\n\n"); e != nil {
				return nil
			}
			if e = rc.Flush(); e != nil {
				return nil
			}
		}
	}
}
