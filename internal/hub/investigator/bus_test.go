package investigator

import (
	"sync"
	"testing"
	"time"
)

func TestBus_PublishSubscribe(t *testing.T) {
	b := NewBus()
	ch, unsub := b.Subscribe("inv1")
	defer unsub()

	b.Publish("inv1", EventMessageAppended, map[string]any{"seq": 1})
	select {
	case ev := <-ch:
		if ev.Type != EventMessageAppended || ev.InvestigationID != "inv1" {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestBus_DropOldestOnBackpressure(t *testing.T) {
	b := NewBus()
	ch, unsub := b.Subscribe("inv1")
	defer unsub()

	// Flood past the buffer without reading — should not deadlock.
	for i := 0; i < subscriptionBufferSize*3; i++ {
		b.Publish("inv1", EventMessageAppended, map[string]any{"seq": i})
	}

	// Drain whatever fits. Must be <= buffer size.
	got := 0
	for {
		select {
		case <-ch:
			got++
		default:
			if got == 0 {
				t.Fatal("got no events at all")
			}
			if got > subscriptionBufferSize {
				t.Fatalf("drained %d > buffer %d", got, subscriptionBufferSize)
			}
			return
		}
	}
}

func TestBus_Unsubscribe(t *testing.T) {
	b := NewBus()
	_, unsub := b.Subscribe("inv1")
	if n := b.SubscriberCount("inv1"); n != 1 {
		t.Fatalf("want 1 sub, got %d", n)
	}
	unsub()
	if n := b.SubscriberCount("inv1"); n != 0 {
		t.Fatalf("want 0 subs after unsubscribe, got %d", n)
	}
}

func TestBus_MultipleSubscribers(t *testing.T) {
	b := NewBus()
	ch1, u1 := b.Subscribe("inv1")
	ch2, u2 := b.Subscribe("inv1")
	defer u1()
	defer u2()

	var wg sync.WaitGroup
	wg.Add(2)
	received := make(chan EventType, 2)
	go func() {
		defer wg.Done()
		ev := <-ch1
		received <- ev.Type
	}()
	go func() {
		defer wg.Done()
		ev := <-ch2
		received <- ev.Type
	}()
	b.Publish("inv1", EventStatusChanged, map[string]any{"status": "done"})
	wg.Wait()
	close(received)
	for ev := range received {
		if ev != EventStatusChanged {
			t.Fatalf("unexpected %v", ev)
		}
	}
}

func TestBus_NilReceiver(t *testing.T) {
	var b *Bus
	ch, unsub := b.Subscribe("inv1")
	b.Publish("inv1", EventMessageAppended, nil) // must not panic
	unsub()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("nil bus should return closed channel")
		}
	default:
		t.Fatal("nil bus channel should be readable (closed)")
	}
}
