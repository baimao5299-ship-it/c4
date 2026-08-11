package user

import (
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// GetUserUsageLogs 我的用量明细（/user/usage_logs；强制 user_id = 当前用户——
// 越权过滤在 service/repo 层，请求侧不可指定他人，ServerInterface）。
func (h *UserAPI) GetUserUsageLogs(w http.ResponseWriter, r *http.Request, params GetUserUsageLogsParams) {
	lq := repository.UsageQuery{Limit: 20, Offset: 0, UserID: currentUserID(r)}
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
