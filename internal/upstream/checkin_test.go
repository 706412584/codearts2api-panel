package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// bothCheckins 默认两个活动都尝试。单测要区分的只是「本地已了结时不要重复领」，
// 所以除专门的用例之外都用它。
var bothCheckins = CheckinOptions{Daily: true, NewUser: true}

func TestCheckinTransitionsAndEligibility(t *testing.T) {
	tests := []struct {
		name, status string
		claimable    bool
		end          string
		start        string
		wantOK       bool
		wantWrites   []string
		wantCredits  float64
	}{
		{name: "fresh", status: "ELIGIBLE", claimable: true, wantOK: true, wantWrites: []string{EpCreditClaim, EpCreditConfirm}, wantCredits: 1000},
		{name: "unconfirmed", status: "CLAIMED", wantOK: true, wantWrites: []string{EpCreditConfirm}},
		{name: "confirmed", status: "CONFIRMED", wantOK: true},
		{name: "consumed", status: "CONSUMED", wantOK: true},
		{name: "not eligible", wantOK: true},
		{name: "not yet claimable", status: "ELIGIBLE"},
		{name: "unknown status", status: "CHANGED"},
		{name: "expired", status: "ELIGIBLE", claimable: true, end: "2020-01-01T00:00:00Z", wantOK: true},
		{name: "not started", status: "ELIGIBLE", claimable: true, start: "2100-01-01T00:00:00+08:00", wantOK: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var writes []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Agent-Type") != "PromptCenter" || r.Header.Get("X-Security-Token") != "token" || !strings.HasPrefix(r.Header.Get("Authorization"), "SDK-HMAC-SHA256 ") {
					t.Error("missing signed PromptCenter credentials")
				}
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					if r.URL.Path != EpCreditDelivery || r.URL.Query().Get("channel") != "IDE" {
						t.Errorf("wrong delivery URL: %s", r.URL)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"items": []any{
						map[string]any{"campaignId": 1, "type": CampaignTypeDailyLogin, "title": "daily", "status": tc.status, "claimable": tc.claimable, "benefitUnit": "CREDIT", "benefitAmount": 1000, "pendingTotalAmount": 1000, "extra": map[string]string{"endTime": tc.end, "startTime": tc.start}},
						map[string]any{"campaignId": 2, "type": "STUDENT_CERTIFIED", "claimable": true, "status": "ELIGIBLE"},
						map[string]any{"campaignId": 3, "type": "INVITE_USER", "claimable": true, "status": "ELIGIBLE"},
					}}})
					return
				}
				writes = append(writes, r.URL.Path)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["campaignId"] != float64(1) {
					t.Errorf("unexpected campaign: %v", body)
				}
				if r.URL.Path == EpCreditClaim {
					key, _ := body["idempotentKey"].(string)
					if body["channel"] != "IDE" || !strings.HasPrefix(key, "claim_1_") {
						t.Errorf("wrong claim body: %v", body)
					}
				}
				_, _ = fmt.Fprint(w, `{"code":0,"message":"ok"}`)
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{AccessKeyID: "ak", SecretAccessKey: "sk", SecurityToken: "token"}, bothCheckins)
			if err != nil || res.OK != tc.wantOK || res.Credits != tc.wantCredits || !reflect.DeepEqual(writes, tc.wantWrites) {
				t.Fatalf("result=%+v err=%v writes=%v, want ok=%t credits=%g writes=%v", res, err, writes, tc.wantOK, tc.wantCredits, tc.wantWrites)
			}
		})
	}
}

func TestCheckinRecoversConfirmationAndDailyReset(t *testing.T) {
	status := "ELIGIBLE"
	claimCount, confirmCount := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCreditDelivery:
			_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":1,"type":%q,"status":%q,"claimable":true,"benefitAmount":1000}]}}`, CampaignTypeDailyLogin, status)
		case EpCreditClaim:
			claimCount++
			status = "CLAIMED"
			_, _ = fmt.Fprint(w, `{"code":0}`)
		case EpCreditConfirm:
			confirmCount++
			if confirmCount == 1 {
				// HTTP 200 的业务失败必须重试，不能当作确认成功。
				_, _ = fmt.Fprint(w, `{"code":40001,"message":"try again"}`)
				return
			}
			status = "CONFIRMED"
			_, _ = fmt.Fprint(w, `{"code":0}`)
		}
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	for i, wantOK := range []bool{false, true, true} {
		res, err := c.Checkin(context.Background(), SignCredential{}, bothCheckins)
		if err != nil || res.OK != wantOK {
			t.Fatalf("attempt %d result=%+v err=%v", i, res, err)
		}
	}
	if claimCount != 1 || confirmCount != 2 {
		t.Fatalf("duplicate or missing write: claim=%d confirm=%d", claimCount, confirmCount)
	}
	// 上游在次日重新变为 ELIGIBLE；本地不得永久缓存「已签」。
	status = "ELIGIBLE"
	res, err := c.Checkin(context.Background(), SignCredential{}, bothCheckins)
	if err != nil || !res.OK || res.Credits != 1000 || claimCount != 2 || confirmCount != 3 {
		t.Fatalf("daily reset not handled: result=%+v err=%v claim=%d confirm=%d", res, err, claimCount, confirmCount)
	}
}

func TestCheckinNewUserAndMalformedResponses(t *testing.T) {
	tests := []struct {
		name, delivery, claim string
		wantErr, wantOK       bool
		wantConfirm           int
		wantCredits           float64
	}{
		{name: "new user", claim: `{"code":0}`, wantOK: true, wantConfirm: 1, wantCredits: 4000},
		{name: "claim business failure", claim: `{"code":40002,"message":"not ready"}`},
		{name: "claim missing code", claim: `{"message":"ok"}`},
		{name: "empty campaigns", delivery: `{"code":0,"data":{"items":[]}}`, wantOK: true},
		{name: "missing items", delivery: `{"code":0,"data":{}}`, wantErr: true},
		{name: "missing business code", delivery: `{"data":{"items":[]}}`, wantErr: true},
		{name: "delivery business failure", delivery: `{"code":40001,"message":"not ready"}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			confirms := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case EpCreditDelivery:
					body := tc.delivery
					if body == "" {
						body = `{"code":0,"data":{"items":[{"campaignId":4,"type":"NEW_USER_REGISTER","title":"new user","status":"ELIGIBLE","claimable":true,"benefitAmount":4000}]}}`
					}
					_, _ = fmt.Fprint(w, body)
				case EpCreditClaim:
					var body struct {
						ID int `json:"campaignId"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body.ID != 4 {
						t.Errorf("claimed other campaign: %d", body.ID)
					}
					_, _ = fmt.Fprint(w, tc.claim)
				case EpCreditConfirm:
					confirms++
					_, _ = fmt.Fprint(w, `{"code":0}`)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{}, bothCheckins)
			if (err != nil) != tc.wantErr || res.OK != tc.wantOK || confirms != tc.wantConfirm || res.Credits != tc.wantCredits {
				t.Fatalf("result=%+v err=%v confirms=%d", res, err, confirms)
			}
		})
	}
}

// 活动 ID 随投放批次变化（2026-11-01 实测每日签到是 campaignId=34，旧投放是 1），
// 判据只能是 type。按写死的 ID 选会在换批后静默「一个活动都选不中」。
func TestCheckinSelectsCampaignsByTypeNotID(t *testing.T) {
	var writes []string
	var claimed []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[
				{"campaignId":1,"title":"decoy","type":"INVITE_USER","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":1000},
				{"campaignId":34,"title":"每日签到领100积分","type":"USER_LOGIN","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":100,"pendingTotalAmount":100},
				{"campaignId":2,"title":"student","type":"STUDENT_CERTIFIED","status":"ELIGIBLE","claimable":true,"benefitUnit":"CREDIT","benefitAmount":4000}
			]}}`)
			return
		}
		writes = append(writes, r.URL.Path)
		var body struct {
			ID int `json:"campaignId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		claimed = append(claimed, body.ID)
		_, _ = fmt.Fprint(w, `{"code":0,"message":"ok"}`)
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	res, err := c.Checkin(context.Background(), SignCredential{}, bothCheckins)
	if err != nil || !res.OK {
		t.Fatalf("result=%+v err=%v", res, err)
	}
	if !reflect.DeepEqual(claimed, []int{34, 34}) {
		t.Fatalf("claimed campaigns = %v, want only the USER_LOGIN campaign 34", claimed)
	}
	if !reflect.DeepEqual(writes, []string{EpCreditClaim, EpCreditConfirm}) || res.Credits != 100 {
		t.Fatalf("writes=%v credits=%g", writes, res.Credits)
	}
}

// 没有签到/新手活动时要说明「未下发」，同时把两个活动都标成 unavailable ——
// 本地据此了结，不必每天再问上游一次；不能让「查到了但没有」与「领完了」同形。
func TestCheckinReportsMissingCheckinActivity(t *testing.T) {
	writes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[{"campaignId":3,"type":"INVITE_USER","claimable":false,"status":null}]}}`)
			return
		}
		writes++
		_, _ = fmt.Fprint(w, `{"code":0}`)
	}))
	defer srv.Close()
	c := New(time.Second)
	c.snapBase = srv.URL
	res, err := c.Checkin(context.Background(), SignCredential{}, bothCheckins)
	if err != nil || !res.OK || writes != 0 || !strings.Contains(res.Message, "未下发") {
		t.Fatalf("result=%+v err=%v writes=%d", res, err, writes)
	}
	if !res.Daily.Settled() || !res.NewUser.Settled() || res.Daily != ClaimOutcomeUnavailable || res.NewUser != ClaimOutcomeUnavailable {
		t.Fatalf("absent campaigns must be settled as unavailable: daily=%q new_user=%q", res.Daily, res.NewUser)
	}
}

// 本地已了结的活动不该再被领取：账号池只把还没了结的那一个打开。
func TestCheckinSkipsSettledCampaigns(t *testing.T) {
	cases := []struct {
		name        string
		opts        CheckinOptions
		wantWrites  []int
		wantDaily   ClaimOutcome
		wantNewUser ClaimOutcome
	}{
		{name: "daily only", opts: CheckinOptions{Daily: true}, wantWrites: []int{34}, wantDaily: ClaimOutcomeClaimed},
		{name: "new user only", opts: CheckinOptions{NewUser: true}, wantWrites: []int{4}, wantNewUser: ClaimOutcomeClaimed},
		{name: "nothing to do", opts: CheckinOptions{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var claimed []int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprint(w, `{"code":0,"data":{"items":[
						{"campaignId":34,"type":"USER_LOGIN","status":"ELIGIBLE","claimable":true,"benefitAmount":100},
						{"campaignId":4,"type":"NEW_USER_REGISTER","status":"ELIGIBLE","claimable":true,"benefitAmount":4000}
					]}}`)
					return
				}
				var body struct {
					ID int `json:"campaignId"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if r.URL.Path == EpCreditClaim {
					claimed = append(claimed, body.ID)
				}
				_, _ = fmt.Fprint(w, `{"code":0}`)
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{}, tc.opts)
			if err != nil || !res.OK {
				t.Fatalf("result=%+v err=%v", res, err)
			}
			if !reflect.DeepEqual(claimed, tc.wantWrites) {
				t.Fatalf("claimed=%v want=%v", claimed, tc.wantWrites)
			}
			if res.Daily != tc.wantDaily || res.NewUser != tc.wantNewUser {
				t.Fatalf("outcome daily=%q new_user=%q want=%q/%q", res.Daily, res.NewUser, tc.wantDaily, tc.wantNewUser)
			}
		})
	}
}

// 每种活动状态对应一个结果类别，账号池靠它决定「明天/下次还要不要再来」。
func TestCheckinOutcomes(t *testing.T) {
	tests := []struct {
		name, status   string
		claimable      bool
		claim, confirm string
		want           ClaimOutcome
	}{
		{name: "fresh claim", status: "ELIGIBLE", claimable: true, claim: `{"code":0}`, confirm: `{"code":0}`, want: ClaimOutcomeClaimed},
		{name: "already confirmed", status: "CONFIRMED", want: ClaimOutcomeAlready},
		{name: "already consumed", status: "CONSUMED", want: ClaimOutcomeAlready},
		{name: "confirm recovered", status: "CLAIMED", confirm: `{"code":0}`, want: ClaimOutcomeClaimed},
		{name: "no qualification", status: "", want: ClaimOutcomeUnavailable},
		{name: "not claimable yet", status: "ELIGIBLE", want: ClaimOutcomeFailed},
		{name: "unknown status", status: "CHANGED", claimable: true, want: ClaimOutcomeFailed},
		{name: "claim rejected as duplicate", status: "ELIGIBLE", claimable: true, claim: `{"code":40001,"message":"并发冲突，请重试"}`, want: ClaimOutcomeAlready},
		{name: "claim other business error", status: "ELIGIBLE", claimable: true, claim: `{"code":40002,"message":"not ready"}`, want: ClaimOutcomeFailed},
		{name: "confirm failed", status: "ELIGIBLE", claimable: true, claim: `{"code":0}`, confirm: `{"code":40001}`, want: ClaimOutcomePending},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet:
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"items":[{"campaignId":34,"type":"USER_LOGIN","status":%q,"claimable":%t,"benefitAmount":100}]}}`, tc.status, tc.claimable)
				case r.URL.Path == EpCreditClaim:
					if tc.claim == "" {
						t.Error("claim must not be attempted")
					}
					_, _ = fmt.Fprint(w, tc.claim)
				case r.URL.Path == EpCreditConfirm:
					if tc.confirm == "" {
						t.Error("confirm must not be attempted")
					}
					_, _ = fmt.Fprint(w, tc.confirm)
				}
			}))
			defer srv.Close()
			c := New(time.Second)
			c.snapBase = srv.URL
			res, err := c.Checkin(context.Background(), SignCredential{}, bothCheckins)
			if err != nil {
				t.Fatal(err)
			}
			if res.Daily != tc.want {
				t.Fatalf("outcome=%q want=%q (result=%+v)", res.Daily, tc.want, res)
			}
		})
	}
}
