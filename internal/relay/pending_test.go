package relay

import "testing"

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
	if len(got) > maxPendingPerAgent {
		t.Fatalf("parked %d events, cap is %d", len(got), maxPendingPerAgent)
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
