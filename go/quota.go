package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// quotaPool is one credit pool of Qoder's usage envelope: the plan's own
// credits, add-on credits and an organisation's shared package.
type quotaPool struct {
	Name      string
	Total     float64
	Used      float64
	Remaining float64
	Unit      string
}

type usageEnvelope struct {
	DisplayMode string `json:"displayMode"`
	ModeSnake   string `json:"display_mode"`
	Usage       *struct {
		UserType     string          `json:"userType"`
		UserTypeAlt  string          `json:"user_type"`
		ExpiresAt    json.RawMessage `json:"expiresAt"`
		ExpiresAtAlt json.RawMessage `json:"expires_at"`
		UserQuota    *usagePool      `json:"userQuota"`
		UserQuotaAlt *usagePool      `json:"user_quota"`
		AddOn        *usagePool      `json:"addOnQuota"`
		AddOnAlt     *usagePool      `json:"add_on_quota"`
		Org          *usagePool      `json:"orgResourcePackage"`
		OrgAlt       *usagePool      `json:"org_resource_package"`
		Dedicated    []usagePool     `json:"dedicatedResourcePackages"`
		DedicatedAlt []usagePool     `json:"dedicated_resource_packages"`
	} `json:"qoderUsage"`
}

type usagePool struct {
	Name      string   `json:"name"`
	Total     *float64 `json:"total"`
	Cap       *float64 `json:"cap"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	Unit      string   `json:"unit"`
}

func (p *usagePool) numbers() (total, used float64, ok bool) {
	if p == nil {
		return 0, 0, false
	}
	if p.Total != nil {
		total = *p.Total
	} else if p.Cap != nil {
		total = *p.Cap
	} else {
		return 0, 0, false
	}
	if total <= 0 {
		return 0, 0, false
	}
	switch {
	case p.Used != nil:
		used = *p.Used
	case p.Remaining != nil:
		used = total - *p.Remaining
	default:
		return 0, 0, false
	}
	if used < 0 {
		used = 0
	}
	return total, used, true
}

func quotaDescription() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{credentialProvider}, DisplayName: "Qoder", SupportsReset: false}
}

func quotaReset() pluginapi.QuotaResetResponse {
	return pluginapi.QuotaResetResponse{Success: false, Message: "Reset Qoder credits on the vendor website"}
}

// quotaFetch reads the account's usage with the device token, which is the one
// the account pages use, and normalizes it.
func quotaFetch(req pluginapi.QuotaFetchRequest, sender httpSender) (pluginapi.QuotaFetchResponse, error) {
	out := pluginapi.QuotaFetchResponse{}
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return out, pluginabiError("invalid_auth", "invalid Qoder credential", 401)
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return out, pluginabiError("invalid_auth", "invalid Qoder credential", 401)
	}
	if strings.TrimSpace(c.DeviceToken) == "" {
		return out, pluginabiError("invalid_auth", "this Qoder credential has no usage token; sign in again", 401)
	}
	raw, status, err := readUsage(sender, spec, c.DeviceToken)
	if err != nil {
		return out, pluginabiError("usage_error", err.Error(), status)
	}
	var env usageEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return out, pluginabiError("usage_error", "Qoder returned an invalid usage response", 502)
	}
	mode := firstNonEmpty(env.DisplayMode, env.ModeSnake)
	if env.Usage == nil || mode == "enterprise" {
		out.Subscription = &pluginapi.QuotaSubscription{Plan: "Enterprise", TierName: "Qoder"}
		return out, nil
	}
	plan := firstNonEmpty(env.Usage.UserType, env.Usage.UserTypeAlt, "Qoder")
	out.Subscription = &pluginapi.QuotaSubscription{Plan: plan, TierName: "Qoder"}
	until := firstNonEmpty(string(env.Usage.ExpiresAt), string(env.Usage.ExpiresAtAlt), "null")
	reset := ""
	if at := whenSaid(json.RawMessage(until)); at > 0 {
		reset = time.UnixMilli(at).UTC().Format(time.RFC3339)
	}
	var pools []quotaPool
	add := func(p *usagePool, fallback string) {
		total, used, ok := p.numbers()
		if !ok {
			return
		}
		name := strings.TrimSpace(p.Name)
		if name == "" {
			name = fallback
		}
		unit := strings.TrimSpace(p.Unit)
		if unit == "" {
			unit = "credits"
		}
		pools = append(pools, quotaPool{Name: name, Total: total, Used: used, Remaining: math.Max(0, total-used), Unit: unit})
	}
	add(first(env.Usage.UserQuota, env.Usage.UserQuotaAlt), "Credits")
	add(first(env.Usage.AddOn, env.Usage.AddOnAlt), "Add-on credits")
	add(first(env.Usage.Org, env.Usage.OrgAlt), "Shared credits")
	dedicated := env.Usage.Dedicated
	if len(dedicated) == 0 {
		dedicated = env.Usage.DedicatedAlt
	}
	for i := range dedicated {
		add(&dedicated[i], "Dedicated credits")
	}
	if len(pools) == 0 {
		// A free account with an empty plan: Qoder's client shows 0 credits,
		// which is no credits left rather than no data.
		pools = append(pools, quotaPool{Name: "Credits", Total: 0, Used: 0, Remaining: 0, Unit: "credits"})
	}
	group := pluginapi.QuotaGroup{DisplayName: "Qoder credits"}
	for _, p := range pools {
		fraction := 0.0
		if p.Total > 0 {
			fraction = clamp01(p.Remaining / p.Total)
		}
		desc := fmt.Sprintf("%s / %s %s left", formatAmount(p.Remaining), formatAmount(p.Total), p.Unit)
		bucket := pluginapi.QuotaBucket{Window: p.Name, RemainingFraction: fraction, Description: desc}
		if p.Name == "Credits" {
			bucket.ResetTime = reset
		}
		group.Buckets = append(group.Buckets, bucket)
		out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: "credits_left", Label: p.Name, Value: p.Remaining, Unit: p.Unit, Format: "number"})
	}
	out.Groups = append(out.Groups, group)
	return out, nil
}

func first[T any](a, b *T) *T {
	if a != nil {
		return a
	}
	return b
}

func readUsage(sender httpSender, spec siteSpec, deviceToken string) ([]byte, int, error) {
	target := spec.OpenAPI + "/sash/api/v2/me/usage"
	resp, err := sender.Do(pluginapi.HTTPRequest{
		Method: "GET",
		URL:    target,
		Headers: http.Header{
			"Accept":          {"application/json"},
			"Authorization":   {"Bearer " + deviceToken},
			"Cosy-ClientType": {"10"},
			"User-Agent":      {"Qoder"},
		},
	})
	if err != nil {
		return nil, http.StatusBadGateway, errors.New("Qoder usage request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, fmt.Errorf("Qoder usage failed (HTTP %d)", resp.StatusCode)
	}
	return resp.Body, 0, nil
}

func formatAmount(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
