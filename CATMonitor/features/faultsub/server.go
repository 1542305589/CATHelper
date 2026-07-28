package faultsub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// apiServer bundles the dependencies the REST handlers need.
type apiServer struct {
	disp   *Dispatcher
	fstore *FaultStorage
	logger *slog.Logger
}

// ServeAPI starts the fault-subscription REST API on addr. It blocks until
// ctx is canceled (the daemon runs it in a goroutine). Mirrors exporter's
// ServeMetrics shape: net/http, //go:embed-free, single mux.
func ServeAPI(ctx context.Context, addr string, disp *Dispatcher, fstore *FaultStorage, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	s := &apiServer{disp: disp, fstore: fstore, logger: logger}
	mux := http.NewServeMux()
	s.register(mux)

	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	logger.Info("faultsub REST listening", "addr", addr)
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logger.Error("faultsub REST server error", "error", err)
	}
}

func (s *apiServer) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /-/healthy", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /-/ready", func(w http.ResponseWriter, r *http.Request) {
		if s.fstore == nil || !s.fstore.Ready() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /faultsub/types", s.handleTypes)
	mux.HandleFunc("GET /faultsub/snapshot", s.handleSnapshot)
	mux.HandleFunc("GET /faultsub/events", s.handleEvents)

	mux.HandleFunc("POST /faultsub/subscriptions", s.handleCreateSub)
	mux.HandleFunc("GET /faultsub/subscriptions", s.handleListSubs)
	mux.HandleFunc("GET /faultsub/subscriptions/{id}", s.handleGetSub)
	mux.HandleFunc("DELETE /faultsub/subscriptions/{id}", s.handleDeleteSub)
}

// writeJSON marshals v and writes it with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *apiServer) handleTypes(w http.ResponseWriter, r *http.Request) {
	types := AllFaultTypes()
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"types": out})
}

func (s *apiServer) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.fstore.Snapshot()
	writeJSON(w, http.StatusOK, snap)
}

func (s *apiServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var since time.Time
	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			errJSON(w, http.StatusBadRequest, "invalid 'since' (use RFC3339)")
			return
		}
		since = t
	}
	events := s.disp.Events(since, q.Get("type"), q.Get("npu_id"))
	if events == nil {
		events = []FaultEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *apiServer) handleCreateSub(w http.ResponseWriter, r *http.Request) {
	var sub Subscription
	if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
		errJSON(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if sub.Delivery == "" {
		sub.Delivery = DeliveryWebhook
	}
	if sub.Delivery == DeliveryWebhook && sub.Endpoint == "" {
		errJSON(w, http.StatusBadRequest, "webhook delivery requires 'endpoint' URL")
		return
	}
	stored := s.disp.Subscriptions().Add(&sub)
	writeJSON(w, http.StatusCreated, stored)
}

func (s *apiServer) handleListSubs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.disp.Subscriptions().All())
}

func (s *apiServer) handleGetSub(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sub := s.disp.Subscriptions().Get(id)
	if sub == nil {
		errJSON(w, http.StatusNotFound, "subscription not found")
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (s *apiServer) handleDeleteSub(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.disp.Subscriptions().Remove(id) {
		errJSON(w, http.StatusNotFound, "subscription not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
