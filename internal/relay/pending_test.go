package relay

import (
	"testing"
	"time"
)

func TestParkEventCoalescesByRef(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}

	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatalf("DrainEvents: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d parked events, want 1 (coalesced)", len(got))
	}
	if string(got[0].Payload) != `{"after":"new"}` {
		t.Fatalf("payload = %s, want the newer one", got[0].Payload)
	}
	if got[0].App != "blog" || got[0].Ref != "main" || got[0].Event != "push" {
		t.Fatalf("event = %+v", got[0])
	}
}

func TestParkEventKeepsDistinctRefs(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := st.ParkEvent(agent, "blog", "pr-7", "pull_request", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
}

func TestDrainEventsEmptiesTheSlot(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")
	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DrainEvents(agent); err != nil {
		t.Fatal(err)
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("drain was not destructive: %+v", got)
	}
}

func TestParkEventCapsPerAgent(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	for i := 0; i < maxPendingPerAgent+10; i++ {
		ref := "pr-" + itoa(i)
		if err := st.ParkEvent(agent, "blog", ref, "pull_request", []byte(`{}`)); err != nil {
			t.Fatalf("ParkEvent %d: %v", i, err)
		}
	}
	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxPendingPerAgent {
		t.Fatalf("parked %d events, want exactly %d", len(got), maxPendingPerAgent)
	}
	// Eviction must drop the OLDEST rows: survivors are pr-10..pr-59, in
	// ascending created_at order (DrainEvents orders by created_at).
	for i, ev := range got {
		want := "pr-" + itoa(i+10)
		if ev.Ref != want {
			t.Fatalf("survivor %d: ref = %s, want %s", i, ev.Ref, want)
		}
	}
}

// TestReparkEventDoesNotClobberNewerEvent pins the coalescing invariant a
// re-park must not break: a replay carrying a stale original created_at must
// lose the slot to a genuinely newer event already parked there.
func TestReparkEventDoesNotClobberNewerEvent(t *testing.T) {
	st := openTestStore(t)
	_, agent := enrolledAgent(t, st, "1001", "alice")

	if err := st.ParkEvent(agent, "blog", "main", "push", []byte(`{"after":"new"}`)); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().UTC().Add(-time.Hour).Format(pendingTimeLayout)
	if err := st.ReparkEvent(agent, "blog", "main", "push", []byte(`{"after":"old"}`), stale); err != nil {
		t.Fatal(err)
	}

	got, err := st.DrainEvents(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	if string(got[0].Payload) != `{"after":"new"}` {
		t.Fatalf("payload = %s, want the newer event to survive the stale re-park", got[0].Payload)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
