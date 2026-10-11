package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"llm-gateway/internal/billing"
	"llm-gateway/internal/provider"
)

const responsesHTTPDelta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"

func withNativeResponses() billingHarnessOption {
	return func(c *billingHarnessConfig) { c.protocol = provider.ProtocolResponses }
}

func responsesHTTPBody(extra string) string {
	return fmt.Sprintf(`{"model":%q,"stream":true,"input":"hello"%s}`, billingTestModel, extra)
}

func responsesHTTPTerminal(kind, usage string) string {
	return fmt.Sprintf("event: response.%s\ndata: {\"type\":\"response.%s\",\"response\":{\"status\":%q,\"usage\":%s}}\n\n", kind, kind, kind, usage)
}

func TestResponsesHTTPReceiptAndSettlement(t *testing.T) {
	for _, kind := range []string{"completed", "incomplete", "failed"} {
		t.Run(kind, func(t *testing.T) {
			terminal := responsesHTTPTerminal(kind, `{"input_tokens":10,"output_tokens":2,"total_tokens":12}`)
			h := newBillingHarness(t, withNativeResponses(), withUpstream(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" {
					t.Errorf("upstream path=%s", r.URL.Path)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if string(body["max_output_tokens"]) != "4096" || string(body["store"]) != "false" || body["max_tokens"] != nil {
					t.Errorf("unbounded upstream body=%v", body)
				}
				sseResponder(responsesHTTPDelta, terminal)(w, r)
			}))
			resp, cancel := h.post(billingTestModel, responsesHTTPBody(""))
			defer cancel()
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK || string(body) != responsesHTTPDelta+terminal {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			receipt := h.receipt(h.singleExecutionID())
			if receipt.Status != billing.ReceiptStatusPendingDispatch || receipt.ChargeMicro != 12 || receipt.TotalTokens != 12 || receipt.PriceVersionID != h.price.ID {
				t.Fatalf("receipt=%+v", receipt)
			}
			params := billing.SettleReceiptParams{Identity: receipt.Identity(), Digest: receipt.Digest, Source: "responses-test-1"}
			if _, err := h.accounts.SettleReceipt(context.Background(), params); err != nil {
				t.Fatal(err)
			}
			if replay, err := h.accounts.SettleReceipt(context.Background(), params); err != nil || !replay.Replayed {
				t.Fatalf("duplicate settlement=%+v err=%v", replay, err)
			}
			account := h.account()
			if account.ReservedMicro != 0 || account.PostedBalanceMicro != 1_000_000-12 {
				t.Fatalf("account=%+v", account)
			}
			if h.count(`SELECT count(*) FROM billing_entries WHERE org_id=$1 AND kind='debit'`, h.orgID) != 1 {
				t.Fatal("charge must occur exactly once")
			}
		})
	}
}

func TestResponsesHTTPUnknownUsageHoldsForReview(t *testing.T) {
	for _, usage := range []string{"null", `{"input_tokens":10}`, `{"input_tokens":"10","output_tokens":2}`, `{"input_tokens":9999999999999999999999,"output_tokens":2}`, `{"input_tokens":10,"output_tokens":2,"total_tokens":0}`, `{"input_tokens":-1,"output_tokens":2,"total_tokens":1}`} {
		t.Run(usage, func(t *testing.T) {
			h := newBillingHarness(t, withNativeResponses(), withUpstream(sseResponder(responsesHTTPDelta, responsesHTTPTerminal("completed", usage))))
			resp, cancel := h.post(billingTestModel, responsesHTTPBody(`,"max_output_tokens":64`))
			defer cancel()
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "response.completed") {
				t.Fatalf("unknown/invalid usage leaked completion: %s", body)
			}
			id := h.singleExecutionID()
			if h.execution(id).Status != billing.StatusReviewRequired || h.receipt(id).Status != billing.ReceiptStatusException || h.account().ReservedMicro <= 0 {
				t.Fatal("uncertain usage must remain held for review")
			}
		})
	}
}

func TestResponsesHTTPBlockingReceiptWithholdsCompletion(t *testing.T) {
	h := newBillingHarness(t, withNativeResponses(), withReceiptBounds(15*time.Second, 5*time.Second, 20*time.Second), withUpstream(sseResponder(responsesHTTPDelta, responsesHTTPTerminal("completed", `{"input_tokens":10,"output_tokens":2,"total_tokens":12}`))))
	release := h.lockReceiptsTable()
	defer release()
	resp, cancel := h.post(billingTestModel, responsesHTTPBody(`,"max_output_tokens":64`))
	defer cancel()
	defer resp.Body.Close()
	reader := newSSEReader(t, resp)
	for i := 0; i < 3; i++ {
		if _, err := reader.next(3 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	h.waitForBlockedReceiptWrite(5 * time.Second)
	reader.expectQuiet(100 * time.Millisecond)
	release()
	if !strings.Contains(reader.drain(5*time.Second), "response.completed") {
		t.Fatal("completion did not arrive after receipt commit")
	}
	if h.receipt(h.singleExecutionID()).Status != billing.ReceiptStatusPendingDispatch {
		t.Fatal("receipt missing")
	}
}

func TestResponsesHTTPRefusalsNeverReserveOrCallUpstream(t *testing.T) {
	h := newBillingHarness(t, withNativeResponses())
	for _, extra := range []string{`,"previous_response_id":"resp_other"`, `,"tools":[{"type":"web_search"}]`, `,"background":true`, `,"max_output_tokens":32769`} {
		resp, cancel := h.post(billingTestModel, responsesHTTPBody(extra))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d extra=%s", resp.StatusCode, extra)
		}
	}
	if len(h.executionIDs()) != 0 || h.upstreamCalls.Load() != 0 || h.account().ReservedMicro != 0 {
		t.Fatal("refused requests touched funds/upstream")
	}
}
