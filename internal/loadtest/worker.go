package loadtest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Result 单次流式请求的观测
type Result struct {
	TTFT    time.Duration // 首字节时间
	Total   time.Duration // 建连到 [DONE] 总时长
	Chunks  int           // 收到多少个 data 行
	Status  int
	Err     error
	Retried bool // 预留：后续做混沌切备统计
}

// Worker 无状态，goroutine 池里复用
type Worker struct {
	client *http.Client
	target string
	apiKey string
	body   []byte // 复用请求模板，避免每次 Marshal
}

func NewWorker(target, apiKey string, body []byte) *Worker {
	return &Worker{
		client: &http.Client{
			// 必须 0 超时，SSE 长连不能被 client 超时掐断
			Timeout: 0,
		},
		target: target,
		apiKey: apiKey,
		body:   body,
	}
}

// Do 发一次 SSE 请求，边读边计 TTFT/抖动
func (w *Worker) Do(ctx context.Context) Result {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.target, bytes.NewReader(w.body))
	if err != nil {
		return Result{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if w.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+w.apiKey)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return Result{Err: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// 读一小段 body 便于定位 429/5xx
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = b
		return Result{Status: resp.StatusCode, Total: time.Since(start)}
	}

	// SSE 是行协议。TTFT 只在收到第一个非空 data event 时记录，不能把
	// comment/空行当成首 token；[DONE] 也不是一个业务 chunk。
	reader := bufio.NewReader(resp.Body)
	var ttft time.Duration
	var chunks int
	var firstDataSeen bool
	var doneSeen bool

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 && bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(data, []byte("[DONE]")) {
				doneSeen = true
				break
			}
			if len(data) > 0 {
				if !firstDataSeen {
					ttft = time.Since(start)
					firstDataSeen = true
				}
				chunks++
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			total := time.Since(start)
			return Result{Status: resp.StatusCode, TTFT: ttft, Total: total, Chunks: chunks, Err: readErr}
		}
	}

	total := time.Since(start)
	if !firstDataSeen {
		ttft = total
	}
	if !doneSeen {
		return Result{Status: resp.StatusCode, TTFT: ttft, Total: total, Chunks: chunks, Err: errors.New("SSE stream ended before [DONE]")}
	}
	return Result{Status: resp.StatusCode, TTFT: ttft, Total: total, Chunks: chunks}
}
