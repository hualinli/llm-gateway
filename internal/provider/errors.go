package provider

import (
	"errors"
	"fmt"
	"net/http"
)

// UpstreamError 是带状态码与重试分类的上游错误。
// 分类决定网关行为：
//   - Retryable=true：5xx / 429 / 408 及网络层错误。尚未向下游写出任何字节时，
//     proxy 可安全切备；
//   - Retryable=false：参数 400、鉴权 401 这类"换一家也一样错"的终态错误，
//     切备只会浪费一次外呼并把配置错误放大成全部 Provider 各打一发。
//
// 未知错误（网络抖动/超时）在未写出窗口内一律按可重试处理。
type UpstreamError struct {
	Status    int    // 上游 HTTP 状态码；0 表示网络层/超时等无状态错误
	Message   string // 面向运维/日志的错误信息，不保证适合直接给客户端
	Retryable bool
}

func (e *UpstreamError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("upstream %d: %s", e.Status, e.Message)
	}
	return "upstream: " + e.Message
}

// IsRetryableStatus 限流与服务端故障值得切备；其余 4xx 是对本请求体的终态错误。
func IsRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return true
	}
	return status >= http.StatusInternalServerError
}

// NewUpstreamError 按状态码自动分类构造。
func NewUpstreamError(status int, msg string) *UpstreamError {
	return &UpstreamError{Status: status, Message: msg, Retryable: IsRetryableStatus(status)}
}

// Retryable 判断错误是否值得重试/切备；未知错误默认按可重试处理。
func Retryable(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Retryable
	}
	return true
}
