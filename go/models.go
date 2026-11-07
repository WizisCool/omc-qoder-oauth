package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// qoderModelSeed is a catalogue entry the plugin ships for a site whose own
// listing cannot be read. It mirrors the fields the request builder needs.
type qoderModelSeed struct {
	Key     string
	Name    string
	Context int64
	Efforts []string
	Images  bool
}

// qoderFallbackCatalogue is Qoder's list at the time of writing. The account's
// own listing takes over as soon as it is read.
var qoderFallbackCatalogue = []qoderModelSeed{
	{Key: "ultimate", Name: "Ultimate", Context: 1_000_000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Images: true},
	{Key: "fast", Name: "Fast", Context: 1_000_000, Images: true},
	{Key: "performance", Name: "Performance", Context: 1_000_000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Images: true},
	{Key: "efficient", Name: "Efficient", Context: 200_000, Images: true},
	{Key: "smodel", Name: "Sonus", Context: 180_000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Images: true},
	{Key: "cmodel", Name: "Cantus", Context: 180_000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Images: true},
	{Key: "qmodel_38max", Name: "Qwen3.8-Max", Context: 180_000, Efforts: []string{"low", "medium", "xhigh"}, Images: true},
	{Key: "qfmodel", Name: "Qwen3.8-Flash", Context: 180_000, Efforts: []string{"low", "medium", "xhigh"}, Images: true},
	{Key: "qmodel_latest", Name: "Qwen3.7-Max", Context: 1_000_000, Images: true},
	{Key: "qmodel", Name: "Qwen3.7-Plus", Context: 1_000_000, Images: true},
	{Key: "kmodel_latest", Name: "Kimi-K3", Context: 180_000, Efforts: []string{"low", "high", "max"}, Images: true},
	{Key: "kmodel", Name: "Kimi-K2.8-Preview", Efforts: []string{"low", "high", "max"}, Images: true},
	{Key: "gmodel", Name: "GLM-5.3", Context: 180_000, Efforts: []string{"low", "high", "max"}, Images: true},
	{Key: "gfmodel", Name: "GLM-5.3-Flash", Context: 1_000_000, Efforts: []string{"high", "max"}, Images: true},
	{Key: "dmodel", Name: "DeepSeek-V4-Pro", Context: 1_000_000, Efforts: []string{"high", "max"}, Images: true},
	{Key: "dfmodel", Name: "DeepSeek-Flash", Context: 1_000_000, Efforts: []string{"low", "high", "max"}, Images: true},
	{Key: "mmodel", Name: "MiniMax-M3", Context: 180_000, Images: true},
}

// qoderCNFallbackCatalogue is what Qoder CN's CLI names: its three tiers and
// Lite. The account's own listing takes over once it is signed in.
var qoderCNFallbackCatalogue = func() []qoderModelSeed {
	var out []qoderModelSeed
	for _, m := range qoderFallbackCatalogue {
		switch m.Key {
		case "ultimate", "performance", "efficient":
			out = append(out, m)
		}
	}
	return append(out, qoderModelSeed{Key: "lite", Name: "Lite"})
}()

// qoderModel is one normalized listing entry.
type qoderModel struct {
	// Key is Qoder's internal catalogue key, and the only name the service
	// routes on. It stays out of published ids because "gmodel" tells a person
	// nothing.
	Key string
	// ID is the readable id CPA publishes and a client names: the catalogue's
	// display name as a lowercase slug ("glm-5.3"). assignIDs keeps it unique.
	ID            string
	Name          string
	Format        string
	Source        string
	Images        bool
	Context       int64
	Windows       []int64
	DefaultWindow int64
	MaxInput      int64
	Thinks        bool
	AlwaysThinks  bool
	Efforts       []string
	DefaultEffort string
	Free          bool
	Rate          float64
	OuterProvider string
}

// listingEntry is one entry of Qoder's model catalogue, in either spelling the
// service uses.
type listingEntry struct {
	Key              string          `json:"key"`
	Format           string          `json:"format"`
	Source           string          `json:"source"`
	Enable           *bool           `json:"enable"`
	DisplayName      string          `json:"display_name"`
	OuterProvider    string          `json:"outer_provider"`
	IsVL             bool            `json:"is_vl"`
	IsReasoning      bool            `json:"is_reasoning"`
	MaxInput         int64           `json:"max_input_tokens"`
	PriceFactor      *float64        `json:"price_factor"`
	PriceFactorCamel *float64        `json:"priceFactor"`
	IsFree           *bool           `json:"is_free"`
	IsFreeCamel      *bool           `json:"isFree"`
	ReasoningEfforts []string        `json:"reasoning_efforts"`
	ThinkingConfig   *thinkingConfig `json:"thinking_config"`
	ContextConfig    map[string]struct {
		TokenCount int64 `json:"token_count"`
		IsDefault  bool  `json:"is_default"`
	} `json:"context_config"`
	ContextConfigCamel map[string]struct {
		TokenCount int64 `json:"token_count"`
		IsDefault  bool  `json:"is_default"`
	} `json:"contextConfig"`
	AvailableWindows []int64 `json:"available_context_windows"`
	DefaultWindow    int64   `json:"default_context_window"`
	Promotion        *struct {
		Active bool     `json:"active"`
		Before *float64 `json:"before_promotion_price_factor"`
	} `json:"promotion"`
}

type thinkingConfig struct {
	Enabled *struct {
		Efforts map[string]struct {
			IsDefault bool `json:"is_default"`
		} `json:"efforts"`
	} `json:"enabled"`
	Disabled json.RawMessage `json:"disabled"`
}

var effortOrder = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

func rankEffort(name string) int {
	for i, v := range effortOrder {
		if v == name {
			return i
		}
	}
	return len(effortOrder)
}

// windowsOf reads the context windows an entry offers.
func windowsOf(raw listingEntry) ([]int64, int64) {
	var windows []int64
	def := int64(0)
	add := func(n int64) bool {
		if n <= 0 {
			return false
		}
		for _, w := range windows {
			if w == n {
				return false
			}
		}
		windows = append(windows, n)
		return true
	}
	config := raw.ContextConfig
	if len(config) == 0 {
		config = raw.ContextConfigCamel
	}
	for _, e := range config {
		add(e.TokenCount)
		if e.IsDefault {
			def = e.TokenCount
		}
	}
	if len(windows) == 0 {
		for _, w := range raw.AvailableWindows {
			add(w)
		}
		def = raw.DefaultWindow
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i] < windows[j] })
	for _, w := range windows {
		if w == def {
			return windows, def
		}
	}
	return windows, 0
}

func priceOf(raw listingEntry) (rate float64, free bool) {
	price := raw.PriceFactor
	if price == nil {
		price = raw.PriceFactorCamel
	}
	if price != nil && !math.IsNaN(*price) {
		if *price != 0 {
			return *price, false
		}
		if p := raw.Promotion; p != nil && p.Active && p.Before != nil && *p.Before > 0 {
			return 0, false
		}
		return 0, true
	}
	isFree := raw.IsFree
	if isFree == nil {
		isFree = raw.IsFreeCamel
	}
	return 0, isFree != nil && *isFree
}

// seedName is the name the shipped catalogue gives a key. A live listing whose
// display name is missing, or merely repeats the key, still gets a name people
// can read.
func seedName(spec siteSpec, key string) string {
	for _, seed := range spec.Fallback {
		if strings.EqualFold(seed.Key, key) {
			return seed.Name
		}
	}
	return ""
}

// normalizeModel turns a listing entry into the shape the request builder uses.
func normalizeModel(raw listingEntry, spec siteSpec) qoderModel {
	windows, def := windowsOf(raw)
	key := strings.TrimSpace(raw.Key)
	name := strings.TrimSpace(raw.DisplayName)
	if name == "" || strings.EqualFold(name, key) {
		name = firstNonEmpty(seedName(spec, key), name, key)
	}
	m := qoderModel{
		Key:           key,
		Name:          name,
		Format:        firstNonEmpty(raw.Format, "openai"),
		Source:        firstNonEmpty(raw.Source, "system"),
		OuterProvider: strings.TrimSpace(raw.OuterProvider),
		Images:        raw.IsVL,
		Windows:       windows,
		DefaultWindow: def,
		MaxInput:      raw.MaxInput,
		Thinks:        raw.IsReasoning,
		Efforts:       append([]string(nil), raw.ReasoningEfforts...),
	}
	if len(windows) > 0 {
		m.Context = windows[len(windows)-1]
	} else {
		m.Context = raw.MaxInput
	}
	if m.DefaultWindow == 0 && raw.MaxInput > 0 {
		m.DefaultWindow = raw.MaxInput
	}
	if tc := raw.ThinkingConfig; tc != nil {
		m.Thinks = tc.Enabled != nil
		m.AlwaysThinks = m.Thinks && len(tc.Disabled) == 0
		m.Efforts = nil
		if tc.Enabled != nil {
			for name, e := range tc.Enabled.Efforts {
				m.Efforts = append(m.Efforts, name)
				if e.IsDefault {
					m.DefaultEffort = name
				}
			}
			sort.Slice(m.Efforts, func(i, j int) bool {
				a, b := rankEffort(m.Efforts[i]), rankEffort(m.Efforts[j])
				if a != b {
					return a < b
				}
				return m.Efforts[i] < m.Efforts[j]
			})
		}
	}
	m.Rate, m.Free = priceOf(raw)
	return m
}

// modelFromSeed is the fallback catalogue entry in the same shape.
func modelFromSeed(s qoderModelSeed) qoderModel {
	return qoderModel{Key: s.Key, ID: publicID(s.Name), Name: s.Name, Format: "openai", Source: "system", Images: s.Images, Context: s.Context, MaxInput: s.Context, Thinks: len(s.Efforts) > 0, Efforts: append([]string(nil), s.Efforts...)}
}

// idSeparators matches everything an id cannot spell: a run of spaces,
// punctuation or non-Latin script becomes one dash.
var idSeparators = regexp.MustCompile(`[^a-z0-9.]+`)

// publicID is the id a catalogue name is published under. The catalogue's
// names are the product names people know, so "GLM-5.3" becomes "glm-5.3":
// lowercase for CPA's case-sensitive registry, and still the model's real name
// in a picker. A name with nothing spellable in it yields "" and the caller
// keeps the key instead.
func publicID(name string) string {
	id := strings.Trim(idSeparators.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-"), "-.")
	if len(id) > 64 {
		id = strings.TrimRight(id[:64], "-.")
	}
	return id
}

// assignIDs gives every published model a unique readable id. The catalogue
// key stays the routing key sent upstream, but it stays free for lookup by the
// model that owns it: a display name that would shadow another entry's key
// falls back to that entry's own key, and two identical display names leave the
// later entry on its key. Order decides, so a catalogue refresh cannot rename a
// model whose neighbours did not change.
func assignIDs(models []qoderModel) []qoderModel {
	reserved := make(map[string]bool, len(models))
	for _, m := range models {
		reserved[strings.ToLower(strings.TrimSpace(m.Key))] = true
	}
	taken := make(map[string]bool, len(models))
	for i := range models {
		key := strings.ToLower(strings.TrimSpace(models[i].Key))
		if key == "" {
			continue
		}
		id := publicID(models[i].Name)
		if id == "" || id == key || taken[id] || reserved[id] {
			id = key
		}
		for n := 2; taken[id]; n++ {
			id = fmt.Sprintf("%s-%d", key, n)
		}
		models[i].ID = id
		taken[id] = true
	}
	return models
}

// matchesModel reports whether a requested name names this model. The readable
// id and the display name are accepted in any casing, and the internal key stays
// accepted so a client written before the ids were readable keeps reaching the
// same catalogue entry.
func matchesModel(m qoderModel, want string) bool {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(m.ID), want) || strings.EqualFold(strings.TrimSpace(m.Key), want) {
		return true
	}
	slug := publicID(want)
	return slug != "" && slug == publicID(m.Name)
}

func fallbackModels(spec siteSpec) []qoderModel {
	out := make([]qoderModel, 0, len(spec.Fallback))
	for _, seed := range spec.Fallback {
		out = append(out, modelFromSeed(seed))
	}
	return assignIDs(out)
}

// sceneModels merges the catalogue's app scene with any chat entry the app no
// longer lists, keeping the app's order and each key once.
func sceneModels(entries map[string][]listingEntry) []listingEntry {
	app := entries["app"]
	chat := entries["chat"]
	out := append([]listingEntry(nil), app...)
	after := -1
	for _, m := range chat {
		found := -1
		for i, x := range out {
			if x.Key == m.Key {
				found = i
				break
			}
		}
		if found >= 0 {
			after = found
			continue
		}
		after++
		out = append(out[:after], append([]listingEntry{m}, out[after:]...)...)
	}
	if len(out) == 0 {
		for _, list := range entries {
			out = append(out, list...)
			break
		}
	}
	return out
}

// usableModels is the catalogue's selectable models: enabled, and never the
// "auto"/"default" keys, which route inside Qoder instead of naming a model.
func usableModels(entries map[string][]listingEntry, spec siteSpec) []qoderModel {
	var out []qoderModel
	seen := map[string]bool{}
	for _, entry := range sceneModels(entries) {
		key := strings.ToLower(strings.TrimSpace(entry.Key))
		if key == "" || key == "auto" || key == "default" || seen[key] {
			continue
		}
		if entry.Enable != nil && !*entry.Enable {
			continue
		}
		seen[key] = true
		out = append(out, normalizeModel(entry, spec))
	}
	return assignIDs(out)
}

func parseListing(body []byte) (map[string][]listingEntry, error) {
	var entries map[string][]listingEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, errors.New("Qoder returned an invalid model catalogue")
	}
	return entries, nil
}

type listingCacheEntry struct {
	at     time.Time
	models []qoderModel
}

var listingCache struct {
	sync.Mutex
	entries map[string]listingCacheEntry
}

const listingTTL = 10 * time.Minute

func listingKey(c credential) string { return c.Site + "\x00" + c.UID }

// fetchListing reads the account's catalogue. A failed read keeps the site's
// shipped list rather than failing the caller.
func fetchListing(sender httpSender, c credential, spec siteSpec, force bool) []qoderModel {
	key := listingKey(c)
	if !force {
		listingCache.Lock()
		if entry, ok := listingCache.entries[key]; ok && time.Since(entry.at) < listingTTL {
			listingCache.Unlock()
			return entry.models
		}
		listingCache.Unlock()
	}
	models, err := readListing(sender, c, spec)
	if err != nil {
		// A stale catalogue still describes the account better than the site's
		// shipped list, so it survives one failed refresh.
		listingCache.Lock()
		entry, ok := listingCache.entries[key]
		listingCache.Unlock()
		if ok && len(entry.models) > 0 {
			return entry.models
		}
		return fallbackModels(spec)
	}
	listingCache.Lock()
	if listingCache.entries == nil {
		listingCache.entries = map[string]listingCacheEntry{}
	}
	listingCache.entries[key] = listingCacheEntry{at: time.Now(), models: models}
	listingCache.Unlock()
	return models
}

func readListing(sender httpSender, c credential, spec siteSpec) ([]qoderModel, error) {
	target := spec.API + "/algo/api/v2/model/list?Encode=1"
	headers, err := cosyHeaders(target, cosyUserFrom(c), "", spec, getConfig().CosyVersion, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	resp, err := sender.Do(pluginapi.HTTPRequest{Method: "GET", URL: target, Headers: headers})
	if err != nil {
		return nil, errors.New("Qoder model catalogue request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Qoder model catalogue failed (HTTP %d)", resp.StatusCode)
	}
	entries, err := parseListing(resp.Body)
	if err != nil {
		return nil, err
	}
	models := usableModels(entries, spec)
	if len(models) == 0 {
		return nil, errors.New("Qoder model catalogue is empty")
	}
	return models, nil
}

// findModel looks a requested model up by published id, by catalogue key or by
// display name, case-insensitively. A miss refreshes the catalogue once before
// falling back to the shipped list.
func findModel(sender httpSender, c credential, spec siteSpec, model string) (qoderModel, bool) {
	look := func(models []qoderModel) (qoderModel, bool) {
		for _, m := range models {
			if matchesModel(m, model) {
				return m, true
			}
		}
		return qoderModel{}, false
	}
	if m, ok := look(fetchListing(sender, c, spec, false)); ok {
		return m, true
	}
	if m, ok := look(fetchListing(sender, c, spec, true)); ok {
		return m, true
	}
	for _, seed := range spec.Fallback {
		m := modelFromSeed(seed)
		if matchesModel(m, model) {
			return m, true
		}
	}
	return qoderModel{}, false
}

func modelInfo(m qoderModel) pluginapi.ModelInfo {
	id := strings.ToLower(strings.TrimSpace(m.ID))
	if id == "" {
		id = strings.ToLower(strings.TrimSpace(m.Key))
	}
	context := m.Context
	if context <= 0 {
		context = m.MaxInput
	}
	info := pluginapi.ModelInfo{
		ID:                         id,
		Name:                       m.Name,
		DisplayName:                m.Name,
		Object:                     "model",
		OwnedBy:                    credentialProvider,
		Type:                       credentialProvider,
		ContextLength:              context,
		InputTokenLimit:            context,
		SupportedInputModalities:   []string{"text"},
		SupportedOutputModalities:  []string{"text"},
		SupportedGenerationMethods: []string{"chat.completions"},
	}
	if m.Images {
		info.SupportedInputModalities = []string{"text", "image"}
	}
	if m.Thinks && len(m.Efforts) > 0 {
		levels := append([]string(nil), m.Efforts...)
		if !m.AlwaysThinks {
			levels = append([]string{"none"}, levels...)
		}
		info.Thinking = &pluginapi.ThinkingSupport{Levels: levels}
	}
	return info
}

func modelResponse(models []qoderModel) pluginapi.ModelResponse {
	out := pluginapi.ModelResponse{Provider: credentialProvider, Models: make([]pluginapi.ModelInfo, 0, len(models))}
	for _, m := range models {
		if strings.TrimSpace(m.Key) == "" {
			continue
		}
		out.Models = append(out.Models, modelInfo(m))
	}
	return out
}

// staticModels publishes the shipped catalogue for the configured fallback site.
func staticModels() pluginapi.ModelResponse {
	spec, ok := siteFor(getConfig().Site)
	if !ok {
		spec, _ = siteFor(siteQoder)
	}
	return modelResponse(fallbackModels(spec))
}

func modelsForAuth(req pluginapi.AuthModelRequest, sender httpSender) (pluginapi.ModelResponse, error) {
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return pluginapi.ModelResponse{}, err
	}
	spec, ok := siteFor(c.Site)
	if !ok {
		return pluginapi.ModelResponse{}, errors.New("invalid qoder credential")
	}
	return modelResponse(fetchListing(sender, c, spec, false)), nil
}

// modelCompiled guards the regexp cache used when a model name arrives with a
// provider prefix.
var modelPrefix = regexp.MustCompile(`^[a-z0-9-]+/(.+)$`)

func trimModelPrefix(model string) string {
	if m := modelPrefix.FindStringSubmatch(model); m != nil {
		return m[1]
	}
	return model
}

// catalogueURL is the endpoint the catalogue is read from, exposed for tests.
func catalogueURL(spec siteSpec) string {
	u, _ := url.Parse(spec.API + "/algo/api/v2/model/list")
	u.RawQuery = url.Values{"Encode": {"1"}}.Encode()
	return u.String()
}
