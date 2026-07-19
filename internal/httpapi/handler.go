package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/charliec05/transactional-reservation-service/internal/reservation"
)

type Handler struct {
	store *reservation.Store
}

func New(store *reservation.Store) http.Handler {
	return &Handler{store: store}
}

func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	path := strings.Trim(request.URL.Path, "/")
	parts := strings.Split(path, "/")

	switch {
	case request.Method == http.MethodGet && path == "healthz":
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	case request.Method == http.MethodGet && path == "metrics":
		h.metrics(writer)
	case request.Method == http.MethodPost && path == "v1/resources":
		h.createResource(writer, request)
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "resources" && request.Method == http.MethodGet:
		h.getResource(writer, parts[2])
	case len(parts) == 4 && parts[0] == "v1" && parts[1] == "resources" && parts[3] == "holds" && request.Method == http.MethodPost:
		h.createHold(writer, request, parts[2])
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "holds" && request.Method == http.MethodGet:
		h.getHold(writer, parts[2])
	case len(parts) == 4 && parts[0] == "v1" && parts[1] == "holds" && parts[3] == "checkout" && request.Method == http.MethodPost:
		h.checkout(writer, parts[2])
	case len(parts) == 3 && parts[0] == "v1" && parts[1] == "holds" && request.Method == http.MethodDelete:
		h.release(writer, parts[2])
	case request.Method == http.MethodGet && path == "v1/events":
		h.events(writer, request)
	default:
		writeJSON(writer, http.StatusNotFound, map[string]string{"error": "route not found"})
	}
}

func (h *Handler) createResource(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ID       string `json:"id"`
		Capacity int    `json:"capacity"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, err)
		return
	}
	resource, err := h.store.AddResource(body.ID, body.Capacity)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, resource)
}

func (h *Handler) getResource(writer http.ResponseWriter, id string) {
	resource, err := h.store.GetResource(id)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, resource)
}

func (h *Handler) createHold(writer http.ResponseWriter, request *http.Request, resourceID string) {
	var body struct {
		Quantity int `json:"quantity"`
		TTLMS    int `json:"ttl_ms"`
	}
	if err := decodeJSON(request, &body); err != nil {
		writeError(writer, err)
		return
	}
	hold, replayed, err := h.store.CreateHold(reservation.CreateHoldRequest{
		ResourceID:     resourceID,
		Quantity:       body.Quantity,
		TTL:            time.Duration(body.TTLMS) * time.Millisecond,
		IdempotencyKey: request.Header.Get("Idempotency-Key"),
	})
	if err != nil {
		writeError(writer, err)
		return
	}
	writer.Header().Set("Idempotent-Replay", strconv.FormatBool(replayed))
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(writer, status, hold)
}

func (h *Handler) getHold(writer http.ResponseWriter, id string) {
	hold, err := h.store.GetHold(id)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, hold)
}

func (h *Handler) checkout(writer http.ResponseWriter, id string) {
	hold, err := h.store.Checkout(id)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, hold)
}

func (h *Handler) release(writer http.ResponseWriter, id string) {
	hold, err := h.store.Release(id)
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, hold)
}

func (h *Handler) events(writer http.ResponseWriter, request *http.Request) {
	after, err := strconv.Atoi(request.URL.Query().Get("after"))
	if err != nil && request.URL.Query().Get("after") != "" {
		writeError(writer, fmt.Errorf("after must be an integer: %w", reservation.ErrInvalidInput))
		return
	}
	writeJSON(writer, http.StatusOK, h.store.Events(after))
}

func (h *Handler) metrics(writer http.ResponseWriter) {
	resources, holds := h.store.Snapshot()
	statusCounts := map[reservation.HoldStatus]int{}
	for _, hold := range holds {
		statusCounts[hold.Status]++
	}
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(writer, "reservation_resources %d\n", len(resources))
	fmt.Fprintf(writer, "reservation_holds_total %d\n", len(holds))
	for _, status := range []reservation.HoldStatus{
		reservation.StatusHeld,
		reservation.StatusCheckedOut,
		reservation.StatusReleased,
		reservation.StatusExpired,
	} {
		fmt.Fprintf(writer, "reservation_holds{status=%q} %d\n", status, statusCounts[status])
	}
	for _, resource := range resources {
		fmt.Fprintf(writer, "reservation_inventory_available{resource=%q} %d\n", resource.ID, resource.Available)
	}
}

func decodeJSON(request *http.Request, destination any) error {
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid JSON body: %w", reservation.ErrInvalidInput)
	}
	return nil
}

func writeError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, reservation.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, reservation.ErrInsufficientStock):
		status = http.StatusConflict
	case errors.Is(err, reservation.ErrIdempotencyConflict):
		status = http.StatusConflict
	case errors.Is(err, reservation.ErrInvalidTransition):
		status = http.StatusConflict
	case errors.Is(err, reservation.ErrInvalidInput):
		status = http.StatusBadRequest
	}
	writeJSON(writer, status, map[string]string{"error": err.Error()})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
