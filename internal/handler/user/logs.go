package user

import (
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// GetUserLogs 我的用量日志（强制 user_id = 当前用户——越权过滤在 service/
// repo 层，请求侧不可指定他人，ServerInterface）。limit/offset 缺省 20/0。
func (h *UserAPI) GetUserLogs(w http.ResponseWriter, r *http.Request, params GetUserLogsParams) {
	lq := repository.LogQuery{Limit: 20, Offset: 0, UserID: currentUserID(r)}
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
		l, ok := item.(*domain.UsageLog)
		if !ok { // 类型不符是内部错误：不能静默丢数据，返回 500
			writeErr(w, http.StatusInternalServerError, "internal error: unexpected log row type")
			return
		}
		out = append(out, toAPIUsageLog(l))
	}
	writeJSON(w, http.StatusOK, LogsResponse{Total: total, Rows: out})
}
