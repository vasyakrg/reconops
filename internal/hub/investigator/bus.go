package investigator

import (
	"encoding/json"
	"sync"
	"time"
)

// EventType enumerates the kinds of investigation events published on the
// Bus. Types intentionally mirror state transitions a remote API client
// (MCP, curl, custom orchestrator) needs to follow without polling.
type EventType string

const (
	EventMessageAppended EventType = "message.appended"
	EventToolCallPending EventType = "tool_call.pending"
	EventToolCallUpdated EventType = "tool_call.updated"
	EventFindingAdded    EventType = "finding.added"
	EventFindingUpdated  EventType = "finding.updated"
	EventStatusChanged   EventType = "status.changed"
	EventBudgetExhausted EventType = "budget.exhausted"
)

// Event is a single pub/sub payload. Data is an already-marshalled JSON
// byte slice so publishers don't pay reflection cost per subscriber and
// subscribers can forward it directly to SSE without re-marshalling.
type Event struct {
	Type            EventType
	InvestigationID string
	Timestamp       time.Time
	Data            json.RawMessage
}

// subscriptionBufferSize is the per-subscriber channel capacity. When full,
// the oldest event is dropped — subscribers that can't keep up should
// reconnect and re-fetch a snapshot. Big enough that a burst of ~200 quick
// messages during compaction doesn't overflow.
const subscriptionBufferSize = 256

// Bus is an in-memory fan-out per investigation_id. Not persistent: if the
// hub restarts, subscribers must reconnect and will receive new events only
// from that point (snapshot on reconnect covers history).
type Bus struct {
	mu     sync.RWMutex
	subs   map[string]map[int]*subscription // invID → subID → sub
	nextID int
}

type subscription struct {
	id int
	ch chan Event
}

// NewBus returns an empty Bus. Safe for zero value too, but explicit for
// wiring clarity in cmd/hub/main.go.
func NewBus() *Bus {
	return &Bus{subs: map[string]map[int]*subscription{}}
}

// Subscribe registers a new listener on invID. Returns the event channel
// and an unsubscribe function the caller MUST defer — otherwise the Bus
// retains the subscription forever.
func (b *Bus) Subscribe(invID string) (<-chan Event, func()) {
	if b == nil {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[invID] == nil {
		b.subs[invID] = map[int]*subscription{}
	}
	b.nextID++
	sub := &subscription{id: b.nextID, ch: make(chan Event, subscriptionBufferSize)}
	b.subs[invID][sub.id] = sub
	unsubscribe := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		bag := b.subs[invID]
		if bag == nil {
			return
		}
		if s, ok := bag[sub.id]; ok {
			delete(bag, sub.id)
			close(s.ch)
		}
		if len(bag) == 0 {
			delete(b.subs, invID)
		}
	}
	return sub.ch, unsubscribe
}

// Publish fans an event out to all subscribers of invID. Non-blocking:
// if a subscriber's channel is full, the oldest event is dropped so the
// publisher never stalls. Safe to call on a nil Bus (no-op).
func (b *Bus) Publish(invID string, typ EventType, data any) {
	if b == nil || invID == "" {
		return
	}
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte("{}")
	}
	ev := Event{
		Type:            typ,
		InvestigationID: invID,
		Timestamp:       time.Now().UTC(),
		Data:            payload,
	}
	b.mu.RLock()
	bag := b.subs[invID]
	subs := make([]*subscription, 0, len(bag))
	for _, s := range bag {
		subs = append(subs, s)
	}
	b.mu.RUnlock()
	for _, s := range subs {
		select {
		case s.ch <- ev:
		default:
			// Drop oldest then append. Single-slot drain is cheap and keeps
			// the channel roughly warm for bursty publishers.
			select {
			case <-s.ch:
			default:
			}
			select {
			case s.ch <- ev:
			default:
			}
		}
	}
}

// SubscriberCount exposes the current number of listeners on invID. Used
// by tests; handlers don't need it.
func (b *Bus) SubscriberCount(invID string) int {
	if b == nil {
		return 0
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs[invID])
}
