package provider

import (
	"io"
	"testing"
)

func TestIsRetryableStatus(t *testing.T) {
	retry := []int{408, 429, 500, 502, 503, 529}
	terminal := []int{400, 401, 402, 403, 404, 413, 422}
	for _, s := range retry {
		if !IsRetryableStatus(s) {
			t.Errorf("status %d: want retryable", s)
		}
	}
	for _, s := range terminal {
		if IsRetryableStatus(s) {
			t.Errorf("status %d: want terminal", s)
		}
	}
}

// 未知错误（网络层/超时）默认按可重试：未写出窗口内切备无副作用。
func TestRetryableDefaultsTrue(t *testing.T) {
	if !Retryable(io.ErrUnexpectedEOF) {
		t.Fatal("unknown error should default to retryable")
	}
	if Retryable(&UpstreamError{Status: 400, Retryable: false}) {
		t.Fatal("explicit terminal must stay terminal")
	}
}
