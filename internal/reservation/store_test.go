package reservation

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentHoldsNeverOversell(t *testing.T) {
	store, err := NewStore("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddResource("class", 50); err != nil {
		t.Fatal(err)
	}

	var successes atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < 200; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, _, err := store.CreateHold(CreateHoldRequest{
				ResourceID:     "class",
				Quantity:       1,
				TTL:            time.Minute,
				IdempotencyKey: fmt.Sprintf("request-%d", index),
			})
			if err == nil {
				successes.Add(1)
				return
			}
			if !errors.Is(err, ErrInsufficientStock) {
				t.Errorf("unexpected error: %v", err)
			}
		}(index)
	}
	wait.Wait()
	if got := successes.Load(); got != 50 {
		t.Fatalf("successful holds = %d, want 50", got)
	}
	resource, err := store.GetResource("class")
	if err != nil {
		t.Fatal(err)
	}
	if resource.Available != 0 {
		t.Fatalf("available = %d, want 0", resource.Available)
	}
	if err := store.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

func TestIdempotentHold(t *testing.T) {
	store, _ := NewStore("", nil)
	_, _ = store.AddResource("seat", 2)
	request := CreateHoldRequest{ResourceID: "seat", Quantity: 1, TTL: time.Minute, IdempotencyKey: "checkout-123"}
	first, replayed, err := store.CreateHold(request)
	if err != nil || replayed {
		t.Fatalf("first create: replayed=%v err=%v", replayed, err)
	}
	second, replayed, err := store.CreateHold(request)
	if err != nil || !replayed {
		t.Fatalf("second create: replayed=%v err=%v", replayed, err)
	}
	if first.ID != second.ID {
		t.Fatalf("hold IDs differ: %s vs %s", first.ID, second.ID)
	}
	resource, _ := store.GetResource("seat")
	if resource.Available != 1 {
		t.Fatalf("available = %d, want 1", resource.Available)
	}

	request.Quantity = 2
	if _, _, err := store.CreateHold(request); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflicting request error = %v", err)
	}
}

func TestExpiryRestoresInventory(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	store, _ := NewStore("", func() time.Time { return now })
	_, _ = store.AddResource("room", 3)
	hold, _, err := store.CreateHold(CreateHoldRequest{
		ResourceID:     "room",
		Quantity:       2,
		TTL:            time.Minute,
		IdempotencyKey: "room-hold",
	})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if count, err := store.ExpireDue(); err != nil || count != 1 {
		t.Fatalf("expired=%d err=%v", count, err)
	}
	resource, _ := store.GetResource("room")
	if resource.Available != 3 {
		t.Fatalf("available = %d, want 3", resource.Available)
	}
	updated, _ := store.GetHold(hold.ID)
	if updated.Status != StatusExpired {
		t.Fatalf("status = %s, want expired", updated.Status)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = store.AddResource("ticket", 10)
	hold, _, err := store.CreateHold(CreateHoldRequest{
		ResourceID:     "ticket",
		Quantity:       3,
		TTL:            time.Hour,
		IdempotencyKey: "durable-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Checkout(hold.ID); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := reloaded.GetResource("ticket")
	if err != nil {
		t.Fatal(err)
	}
	if resource.Available != 7 {
		t.Fatalf("available after restart = %d, want 7", resource.Available)
	}
	reloadedHold, err := reloaded.GetHold(hold.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloadedHold.Status != StatusCheckedOut {
		t.Fatalf("status after restart = %s", reloadedHold.Status)
	}
	if err := reloaded.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}
