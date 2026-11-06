package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaSender answers every usage request with one status and body.
func quotaSender(status int, body string) *fakeSender {
	return &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{StatusCode: status, Body: []byte(body)}, nil
	}}
}

func TestQuotaFetchReadsUsageWithTheDeviceToken(t *testing.T) {
	sender := quotaSender(200, `{"displayMode":"qoder","qoderUsage":{}}`)
	if _, err := quotaFetch(pluginapi.QuotaFetchRequest{Provider: credentialProvider, StorageJSON: testCredential(siteQoder)}, sender); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("usage requests = %d, want 1", sender.count())
	}
	req := sender.calls[0]
	if req.Method != "GET" || req.URL != "https://openapi.qoder.sh/sash/api/v2/me/usage" {
		t.Fatalf("request = %s %s", req.Method, req.URL)
	}
	if req.Headers.Get("Authorization") != "Bearer dt-test-token" {
		t.Fatalf("authorization = %q", req.Headers.Get("Authorization"))
	}
	// the usage endpoint's headers are set as written, not through Header.Set
	if got := req.Headers["Cosy-ClientType"]; len(got) != 1 || got[0] != "10" {
		t.Fatalf("client type = %v", got)
	}
	if req.Headers.Get("User-Agent") != "Qoder" || req.Headers.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", req.Headers)
	}
}

func TestQuotaFetchNormalizesUsage(t *testing.T) {
	resetFromEpoch := time.UnixMilli(1790000000000).UTC().Format(time.RFC3339)
	cases := []struct {
		name string
		body string
		want pluginapi.QuotaFetchResponse
	}{
		{
			name: "qoder usage",
			body: `{"displayMode":"qoder","qoderUsage":{"userType":"personal_pro","expiresAt":"2026-02-03T04:05:06.789Z","userQuota":{"name":"Credits","total":2000,"used":1880,"remaining":120,"unit":"credits"}}}`,
			want: pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{Plan: "personal_pro", TierName: "Qoder"},
				Summary: []pluginapi.QuotaMetric{
					{Key: "credits_left", Label: "Credits", Value: 120, Unit: "credits", Format: "number"},
				},
				Groups: []pluginapi.QuotaGroup{{
					DisplayName: "Qoder credits",
					Buckets: []pluginapi.QuotaBucket{
						{Window: "Credits", RemainingFraction: 0.06, ResetTime: "2026-02-03T04:05:06Z", Description: "120 / 2,000 credits left"},
					},
				}},
			},
		},
		{
			name: "add-on shared and dedicated pools",
			body: `{"displayMode":"qoder","qoderUsage":{"userType":"personal_pro","userQuota":{"total":1000,"used":1000,"unit":"credits"},"addOnQuota":{"total":500,"used":100},"orgResourcePackage":{"name":"Team credits","cap":250,"remaining":50},"dedicatedResourcePackages":[{"total":40,"used":10}]}}`,
			want: pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{Plan: "personal_pro", TierName: "Qoder"},
				Summary: []pluginapi.QuotaMetric{
					{Key: "credits_left", Label: "Credits", Value: 0, Unit: "credits", Format: "number"},
					{Key: "credits_left", Label: "Add-on credits", Value: 400, Unit: "credits", Format: "number"},
					{Key: "credits_left", Label: "Team credits", Value: 50, Unit: "credits", Format: "number"},
					{Key: "credits_left", Label: "Dedicated credits", Value: 30, Unit: "credits", Format: "number"},
				},
				Groups: []pluginapi.QuotaGroup{{
					DisplayName: "Qoder credits",
					Buckets: []pluginapi.QuotaBucket{
						{Window: "Credits", RemainingFraction: 0, Description: "0 / 1,000 credits left"},
						{Window: "Add-on credits", RemainingFraction: 0.8, Description: "400 / 500 credits left"},
						{Window: "Team credits", RemainingFraction: 0.2, Description: "50 / 250 credits left"},
						{Window: "Dedicated credits", RemainingFraction: 0.75, Description: "30 / 40 credits left"},
					},
				}},
			},
		},
		{
			name: "enterprise",
			body: `{"displayMode":"enterprise","qoderUsage":{"userType":"enterprise_pro","userQuota":{"total":1000,"used":200}}}`,
			want: pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{Plan: "Enterprise", TierName: "Qoder"},
			},
		},
		{
			name: "free account counts as out of credits",
			body: `{"displayMode":"qoder","qoderUsage":{"userType":"personal_standard","expiresAt":1790000000000,"userQuota":{"name":"Credits","total":0,"cap":0,"used":0,"remaining":0}}}`,
			want: pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{Plan: "personal_standard", TierName: "Qoder"},
				Summary: []pluginapi.QuotaMetric{
					{Key: "credits_left", Label: "Credits", Value: 0, Unit: "credits", Format: "number"},
				},
				Groups: []pluginapi.QuotaGroup{{
					DisplayName: "Qoder credits",
					Buckets: []pluginapi.QuotaBucket{
						{Window: "Credits", RemainingFraction: 0, ResetTime: resetFromEpoch, Description: "0 credits left"},
					},
				}},
			},
		},
		{
			name: "absent pools count as out of credits",
			body: `{"displayMode":"qoder","qoderUsage":{"userType":"personal_standard"}}`,
			want: pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{Plan: "personal_standard", TierName: "Qoder"},
				Summary: []pluginapi.QuotaMetric{
					{Key: "credits_left", Label: "Credits", Value: 0, Unit: "credits", Format: "number"},
				},
				Groups: []pluginapi.QuotaGroup{{
					DisplayName: "Qoder credits",
					Buckets:     []pluginapi.QuotaBucket{{Window: "Credits", RemainingFraction: 0, Description: "0 credits left"}},
				}},
			},
		},
		{
			name: "snake case envelope",
			body: `{"display_mode":"qoder","qoder_usage":{"user_type":"personal_ultra","expires_at":"2026-05-06T07:08:09Z","user_quota":{"total":2000,"used":1400,"unit":"credits"},"add_on_quota":{"total":300,"used":300,"unit":"credits"},"org_resource_package":{"name":"Org credits","total":100,"used":25},"dedicated_resource_packages":[{"name":"Dedicated A","total":40,"used":10}]}}`,
			want: pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{Plan: "personal_ultra", TierName: "Qoder"},
				Summary: []pluginapi.QuotaMetric{
					{Key: "credits_left", Label: "Credits", Value: 600, Unit: "credits", Format: "number"},
					{Key: "credits_left", Label: "Add-on credits", Value: 0, Unit: "credits", Format: "number"},
					{Key: "credits_left", Label: "Org credits", Value: 75, Unit: "credits", Format: "number"},
					{Key: "credits_left", Label: "Dedicated A", Value: 30, Unit: "credits", Format: "number"},
				},
				Groups: []pluginapi.QuotaGroup{{
					DisplayName: "Qoder credits",
					Buckets: []pluginapi.QuotaBucket{
						{Window: "Credits", RemainingFraction: 0.3, ResetTime: "2026-05-06T07:08:09Z", Description: "600 / 2,000 credits left"},
						{Window: "Add-on credits", RemainingFraction: 0, Description: "0 / 300 credits left"},
						{Window: "Org credits", RemainingFraction: 0.75, Description: "75 / 100 credits left"},
						{Window: "Dedicated A", RemainingFraction: 0.75, Description: "30 / 40 credits left"},
					},
				}},
			},
		},
		{
			name: "missing plan name and unit fall back",
			body: `{"displayMode":"qoder","qoderUsage":{"userQuota":{"total":12.5,"used":2.5}}}`,
			want: pluginapi.QuotaFetchResponse{
				Subscription: &pluginapi.QuotaSubscription{Plan: "Qoder", TierName: "Qoder"},
				Summary: []pluginapi.QuotaMetric{
					{Key: "credits_left", Label: "Credits", Value: 10, Unit: "credits", Format: "number"},
				},
				Groups: []pluginapi.QuotaGroup{{
					DisplayName: "Qoder credits",
					Buckets: []pluginapi.QuotaBucket{
						{Window: "Credits", RemainingFraction: 0.8, Description: "10 / 12.5 credits left"},
					},
				}},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sender := quotaSender(200, c.body)
			got, err := quotaFetch(pluginapi.QuotaFetchRequest{Provider: credentialProvider, StorageJSON: testCredential(siteQoder)}, sender)
			if err != nil {
				t.Fatalf("quotaFetch: %v", err)
			}
			if !reflect.DeepEqual(got.Subscription, c.want.Subscription) {
				t.Errorf("subscription = %+v, want %+v", got.Subscription, c.want.Subscription)
			}
			if !reflect.DeepEqual(got.Summary, c.want.Summary) {
				t.Errorf("summary = %+v, want %+v", got.Summary, c.want.Summary)
			}
			if !reflect.DeepEqual(got.Groups, c.want.Groups) {
				t.Errorf("groups = %+v, want %+v", got.Groups, c.want.Groups)
			}
		})
	}
}

func TestQuotaFetchRefusesWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name      string
		storage   []byte
		status    int
		body      string
		wantHTTP  int
		wantCode  string
		wantInMsg string
		wantCalls int
	}{
		{
			name:      "unknown credential",
			storage:   []byte(`{"site":"qoder","access":"jt-test-token"}`),
			status:    200,
			body:      `{}`,
			wantHTTP:  401,
			wantCode:  "invalid_auth",
			wantInMsg: "invalid Qoder credential",
		},
		{
			name:      "no usage token",
			storage:   storageOf(credential{Site: siteQoder, Access: "jt-test-token", UID: "user-1", MachineID: "machine-1"}),
			status:    200,
			body:      `{}`,
			wantHTTP:  401,
			wantCode:  "invalid_auth",
			wantInMsg: "sign in again",
		},
		{
			name:      "upstream refuses the token",
			status:    401,
			body:      `{"message":"unauthorized"}`,
			wantHTTP:  401,
			wantCode:  "usage_error",
			wantInMsg: "HTTP 401",
			wantCalls: 1,
		},
		{
			name:      "upstream failure",
			status:    500,
			body:      `{"message":"boom"}`,
			wantHTTP:  500,
			wantCode:  "usage_error",
			wantInMsg: "HTTP 500",
			wantCalls: 1,
		},
		{
			name:      "malformed json",
			status:    200,
			body:      `{"displayMode":`,
			wantHTTP:  502,
			wantCode:  "usage_error",
			wantInMsg: "invalid usage response",
			wantCalls: 1,
		},
		{
			name:      "refusal envelope",
			status:    200,
			body:      `{"message":"unauthorized"}`,
			wantHTTP:  502,
			wantCode:  "usage_error",
			wantInMsg: "unknown usage display mode",
			wantCalls: 1,
		},
		{
			name:      "qoder mode without usage",
			status:    200,
			body:      `{"displayMode":"qoder"}`,
			wantHTTP:  502,
			wantCode:  "usage_error",
			wantInMsg: "no quota data",
			wantCalls: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			storage := c.storage
			if storage == nil {
				storage = testCredential(siteQoder)
			}
			sender := quotaSender(c.status, c.body)
			resp, err := quotaFetch(pluginapi.QuotaFetchRequest{Provider: credentialProvider, StorageJSON: storage}, sender)
			if err == nil {
				t.Fatalf("accepted: %+v", resp)
			}
			var e *pluginabi.Error
			if !errors.As(err, &e) {
				t.Fatalf("error %v is not a pluginabi error", err)
			}
			if e.HTTPStatus != c.wantHTTP || e.Code != c.wantCode {
				t.Fatalf("error = %+v, want status %d code %s", e, c.wantHTTP, c.wantCode)
			}
			if !strings.Contains(e.Message, c.wantInMsg) {
				t.Fatalf("message = %q, want it to mention %q", e.Message, c.wantInMsg)
			}
			if resp.Subscription != nil || len(resp.Groups) != 0 || len(resp.Summary) != 0 {
				t.Fatalf("a refusal invented a plan: %+v", resp)
			}
			if sender.count() != c.wantCalls {
				t.Fatalf("usage requests = %d, want %d", sender.count(), c.wantCalls)
			}
		})
	}
}

func TestQuotaFetchReportsTransportFailure(t *testing.T) {
	sender := &fakeSender{reply: func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return pluginapi.HTTPResponse{}, errors.New("dial failed")
	}}
	_, err := quotaFetch(pluginapi.QuotaFetchRequest{Provider: credentialProvider, StorageJSON: testCredential(siteQoder)}, sender)
	var e *pluginabi.Error
	if !errors.As(err, &e) || e.HTTPStatus != 502 || e.Code != "usage_error" {
		t.Fatalf("err = %v", err)
	}
}

func TestFormatAmountWritesCreditCounts(t *testing.T) {
	cases := map[float64]string{
		0:       "0",
		120:     "120",
		2000:    "2,000",
		1234567: "1,234,567",
		1234.5:  "1,234.5",
	}
	for value, want := range cases {
		if got := formatAmount(value); got != want {
			t.Errorf("formatAmount(%v) = %q, want %q", value, got, want)
		}
	}
}
