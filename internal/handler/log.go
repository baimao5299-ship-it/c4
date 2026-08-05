package handler

import (
	"net/http"
	"strconv"
	"time"

	"go-proxy-mini/internal/repository"
)

func (h *Handler) queryLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lq := repository.LogQuery{
		Limit:  parseIntDefault(q.Get("limit"), 20),
		Offset: parseIntDefault(q.Get("offset"), 0),
	}
	if v := q.Get("group_id"); v != "" {
		lq.GroupID = mustI64(v)
	}
	if v := q.Get("account_id"); v != "" {
		lq.AccountID = mustI64(v)
	}
	if v := q.Get("model"); v != "" {
		lq.Model = v
	}
	if v := q.Get("status_code"); v != "" {
		lq.StatusCode = int(mustI64(v)) // LogQuery.StatusCode 为 int（计划/brief 原代码为 int64 赋值，编译修正）
	}
	if v := q.Get("error_type"); v != "" {
		lq.ErrorType = v
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			lq.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			lq.To = &t
		}
	}
	rows, total, err := h.svc.QueryLogs(r.Context(), lq)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "rows": rows})
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

func mustI64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}
