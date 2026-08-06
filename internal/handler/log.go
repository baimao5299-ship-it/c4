package handler

import (
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// GetLogs 用量日志分页查询（ServerInterface）。limit/offset 缺省 20/0
// （契约 default），其余过滤参数仅非 nil 时生效。
func (h *AdminAPI) GetLogs(w http.ResponseWriter, r *http.Request, params GetLogsParams) {
	lq := repository.LogQuery{Limit: 20, Offset: 0}
	if params.Limit != nil {
		lq.Limit = *params.Limit
	}
	if params.Offset != nil {
		lq.Offset = *params.Offset
	}
	if params.GroupId != nil {
		lq.GroupID = *params.GroupId
	}
	if params.AccountId != nil {
		lq.AccountID = *params.AccountId
	}
	if params.Model != nil {
		lq.Model = *params.Model
	}
	if params.StatusCode != nil {
		lq.StatusCode = *params.StatusCode
	}
	if params.ErrorType != nil {
		lq.ErrorType = *params.ErrorType
	}
	if params.From != nil {
		lq.From = params.From
	}
	if params.To != nil {
		lq.To = params.To
	}
	rows, total, err := h.svc.QueryLogs(r.Context(), lq)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]UsageLog, 0, len(rows))
	for _, item := range rows { // service.QueryLogs 返回 []any（元素为 *domain.UsageLog）
		if l, ok := item.(*domain.UsageLog); ok {
			out = append(out, toAPIUsageLog(l))
		}
	}
	writeJSON(w, http.StatusOK, LogsResponse{Total: total, Rows: out})
}
