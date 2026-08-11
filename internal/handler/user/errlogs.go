package user

import (
	"net/http"

	"go-proxy-mini/internal/domain"
	"go-proxy-mini/internal/repository"
)

// GetUserErrLogs 我的错误明细（/user/err_logs：完整错误面——本地拒绝 + 半异常
// 双轨；强制 user_id = 当前用户，防越权）。
func (h *UserAPI) GetUserErrLogs(w http.ResponseWriter, r *http.Request, params GetUserErrLogsParams) {
	lq := repository.ErrLogQuery{Limit: 20, Offset: 0, UserID: currentUserID(r)}
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
	rows, total, err := h.svc.QueryErrLogs(r.Context(), lq)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]ErrLog, 0, len(rows))
	for _, item := range rows { // service.QueryErrLogs 返回 []any（元素为 *domain.UsageLog）
		l, ok := item.(*domain.UsageLog)
		if !ok { // 类型不符是内部错误：不能静默丢数据，返回 500
			writeErr(w, http.StatusInternalServerError, "internal error: unexpected err log row type")
			return
		}
		out = append(out, toAPIErrLog(l))
	}
	writeJSON(w, http.StatusOK, ErrLogsResponse{Total: total, Rows: out})
}
