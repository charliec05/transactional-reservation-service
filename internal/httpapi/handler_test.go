package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/charliec05/transactional-reservation-service/internal/reservation"
)

func TestReservationLifecycle(t *testing.T) {
	store, _ := reservation.NewStore("", nil)
	handler := New(store)

	response := perform(handler, http.MethodPost, "/v1/resources", `{"id":"workout","capacity":2}`, nil)
	if response.Code != http.StatusCreated {
		t.Fatalf("create resource status = %d body=%s", response.Code, response.Body.String())
	}
	response = perform(handler, http.MethodPost, "/v1/resources/workout/holds", `{"quantity":1,"ttl_ms":60000}`, map[string]string{"Idempotency-Key": "request-1"})
	if response.Code != http.StatusCreated {
		t.Fatalf("create hold status = %d body=%s", response.Code, response.Body.String())
	}
	var hold reservation.Hold
	if err := json.Unmarshal(response.Body.Bytes(), &hold); err != nil {
		t.Fatal(err)
	}
	response = perform(handler, http.MethodPost, "/v1/holds/"+hold.ID+"/checkout", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("checkout status = %d body=%s", response.Code, response.Body.String())
	}
	response = perform(handler, http.MethodGet, "/v1/resources/workout", "", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("get resource status = %d", response.Code)
	}
	var resource reservation.Resource
	_ = json.Unmarshal(response.Body.Bytes(), &resource)
	if resource.Available != 1 {
		t.Fatalf("available = %d, want 1", resource.Available)
	}
}

func TestIdempotencyReplayHeader(t *testing.T) {
	store, _ := reservation.NewStore("", nil)
	_, _ = store.AddResource("seat", 1)
	handler := New(store)
	headers := map[string]string{"Idempotency-Key": "same-request"}
	first := perform(handler, http.MethodPost, "/v1/resources/seat/holds", `{"quantity":1,"ttl_ms":60000}`, headers)
	second := perform(handler, http.MethodPost, "/v1/resources/seat/holds", `{"quantity":1,"ttl_ms":60000}`, headers)
	if first.Code != http.StatusCreated || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d", first.Code, second.Code)
	}
	if second.Header().Get("Idempotent-Replay") != "true" {
		t.Fatalf("missing replay header")
	}
}

func perform(handler http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
