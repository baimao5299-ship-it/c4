package handler

import (
	"net/http"
	"time"

	"go-proxy-mini/internal/repository"
)

func (h *Handler) queryStats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sq := repository.StatQuery{From: time.Now().Add(-24 * time.Hour), To: time.Now()}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			sq.From = t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			sq.To = t
		}
	}
	if v := q.Get("group_id"); v != "" {
		sq.GroupID = mustI64(v)
	}
	if v := q.Get("account_id"); v != "" {
		sq.AccountID = mustI64(v)
	}
	if v := q.Get("model"); v != "" {
		sq.Model = v
	}
	granularity := q.Get("granularity")
	if granularity != "hour" && granularity != "day" {
		granularity = "day"
	}
	rows, err := h.svc.QueryStats(r.Context(), sq, granularity)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
