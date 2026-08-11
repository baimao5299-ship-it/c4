package handler

import (
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// GetUsageLogs 用量明细分页查询（/usage_logs；消费面改名裁决：/logs → /usage_logs
// 与表名一致）。limit/offset 缺省 20/0（契约 default），其余过滤参数仅非 nil 时
// 生效。error_type 过滤保留——usage_logs 只剩 abort/failover 半异常标记（错误
// 审计面在 /err_logs）。
func (h *AdminAPI) GetUsageLogs(w http.ResponseWriter, r *http.Request, params GetUsageLogsParams) {
	lq := repository.UsageQuery{Limit: 20, Offset: 0}
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
	if params.UserId != nil {
		lq.UserID = *params.UserId
	}
	if params.Model != nil {
		lq.Model = *params.Model
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
	rows, total, err := h.svc.QueryUsages(r.Context(), lq)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]UsageLog, 0, len(rows))
	for _, item := range rows { // service.QueryUsages 返回 []any（元素为 *domain.UsageLog）
		l, ok := item.(*domain.UsageLog)
		if !ok { // 类型不符是内部错误：不能静默丢数据，返回 500
			writeErr(w, http.StatusInternalServerError, "internal error: unexpected usage log row type")
			return
		}
		out = append(out, toAPIUsageLog(l))
	}
	writeJSON(w, http.StatusOK, LogsResponse{Total: total, Rows: out})
}
