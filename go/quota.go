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
	// Plan marks the plan's own credits, the pool Qoder refreshes when the plan does.
	Plan bool
}

// usageEnvelope is the usage endpoint's answer: the panel it tells the account
// to show, and for a personal account the plan's own credit pools.
type usageEnvelope struct {
	DisplayMode string     `json:"displayMode"`
	ModeSnake   string     `json:"display_mode"`
	Usage       *usageNode `json:"qoderUsage"`
	UsageSnake  *usageNode `json:"qoder_usage"`
}

// usageNode is qoderUsage: the account's plan and every pool of credits it holds.
type usageNode struct {
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
	// The envelope names the panel to show, and an answer that names neither is
	// refused rather than read as a plan: an error envelope carries no mode.
	mode := strings.ToLower(firstNonEmpty(env.DisplayMode, env.ModeSnake))
	switch mode {
	case "enterprise":
		out.Subscription = &pluginapi.QuotaSubscription{Plan: "Enterprise", TierName: "Qoder"}
		return out, nil
	case "qoder":
		if first(env.Usage, env.UsageSnake) == nil {
			return out, pluginabiError("usage_error", "Qoder returned no quota data", 502)
		}
	default:
		return out, pluginabiError("usage_error", fmt.Sprintf("Qoder returned an unknown usage display mode %q", mode), 502)
	}
	usage := first(env.Usage, env.UsageSnake)
	plan := firstNonEmpty(usage.UserType, usage.UserTypeAlt, "Qoder")
	out.Subscription = &pluginapi.QuotaSubscription{Plan: plan, TierName: "Qoder"}
	reset := ""
	if at := expiresAt(usage.ExpiresAt, usage.ExpiresAtAlt); at > 0 {
		reset = time.UnixMilli(at).UTC().Format(time.RFC3339)
	}
	var pools []quotaPool
	add := func(p *usagePool, fallback string, plan bool) {
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
		pools = append(pools, quotaPool{Name: name, Total: total, Used: used, Remaining: math.Max(0, total-used), Unit: unit, Plan: plan})
	}
	add(first(usage.UserQuota, usage.UserQuotaAlt), "Credits", true)
	add(first(usage.AddOn, usage.AddOnAlt), "Add-on credits", false)
	add(first(usage.Org, usage.OrgAlt), "Shared credits", false)
	dedicated := usage.Dedicated
	if len(dedicated) == 0 {
		dedicated = usage.DedicatedAlt
	}
	for i := range dedicated {
		add(&dedicated[i], "Dedicated credits", false)
	}
	group := pluginapi.QuotaGroup{DisplayName: "Qoder credits"}
	if len(pools) == 0 {
		// A free account with an empty plan: Qoder's client counts that pool as
		// 0/0 as out of credits, so this is no credits left rather than no data.
		group.Buckets = append(group.Buckets, pluginapi.QuotaBucket{Window: "Credits", Description: "0 credits left", ResetTime: reset})
		out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: "credits_left", Label: "Credits", Unit: "credits", Format: "number"})
		out.Groups = append(out.Groups, group)
		return out, nil
	}
	for _, p := range pools {
		bucket := pluginapi.QuotaBucket{
			Window:            p.Name,
			RemainingFraction: clamp01(p.Remaining / p.Total),
			Description:       fmt.Sprintf("%s / %s %s left", formatAmount(p.Remaining), formatAmount(p.Total), p.Unit),
		}
		if p.Plan {
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

// formatAmount writes a credit count as Qoder's own panel does: whole units with
// thousands separators, one decimal for a part unit.
func formatAmount(v float64) string {
	var text string
	if v == math.Trunc(v) {
		text = fmt.Sprintf("%.0f", v)
	} else {
		text = fmt.Sprintf("%.1f", v)
	}
	whole, fraction, hasFraction := strings.Cut(text, ".")
	var b strings.Builder
	for i := 0; i < len(whole); i++ {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteByte(whole[i])
	}
	if hasFraction {
		b.WriteByte('.')
		b.WriteString(fraction)
	}
	return b.String()
}

func clamp01(v float64) float64 {
	return math.Max(0, math.Min(1, v))
}
