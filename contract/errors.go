package contract

import "errors"

// ErrorDetail 是可选的机器可读错误信息。Error 字段仍是兼容展示文本。
type ErrorDetail struct {
	Code      string `json:"code"`
	Phase     string `json:"phase"`
	Plugin    string `json:"plugin,omitempty"`
	Fn        string `json:"fn,omitempty"`
	Path      string `json:"path,omitempty"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

// CallError 保留结构化错误,同时实现普通 error 文本契约。
type CallError struct{ Detail ErrorDetail }

func (e *CallError) Error() string { return e.Detail.Message }

// NewCallError 构造带结构化详情的错误。
func NewCallError(detail ErrorDetail) *CallError { return &CallError{Detail: detail} }

// ErrorDetailOf 从错误链提取结构化详情;没有详情时返回 false。
func ErrorDetailOf(err error) (ErrorDetail, bool) {
	var detail ErrorDetail
	if err == nil {
		return detail, false
	}
	var typed *CallError
	if !errors.As(err, &typed) || typed == nil {
		return detail, false
	}
	return typed.Detail, true
}
