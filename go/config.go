package main

import (
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type config struct {
	// Site is the fallback portal for a login request that names no entry.
	Site string `yaml:"site"`
	// MaxConcurrency bounds simultaneous requests per credential.
	MaxConcurrency int `yaml:"max_concurrency"`
	// CosyVersion is the Qoder client build the COSY envelope claims.
	CosyVersion string `yaml:"cosy_version"`
	// MaxTokens is used when the client names no output budget.
	MaxTokens int `yaml:"max_tokens"`
}

var configMu sync.RWMutex
var currentConfig = defaults()

func defaults() config {
	return config{Site: siteQoder, MaxConcurrency: 3, CosyVersion: defaultCosyVersion, MaxTokens: 32000}
}

func getConfig() config { configMu.RLock(); defer configMu.RUnlock(); return currentConfig }

func configure(raw []byte) error {
	c := defaults()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("invalid plugin configuration")
		}
	}
	c.Site = strings.ToLower(strings.TrimSpace(c.Site))
	if c.Site == "" {
		c.Site = siteQoder
	}
	if c.Site != siteQoder && c.Site != siteQoderCN {
		return fmt.Errorf("invalid qoder plugin configuration: site must be qoder or qoder-cn")
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 100 {
		return fmt.Errorf("invalid qoder plugin configuration: max_concurrency must be 1-100")
	}
	c.CosyVersion = strings.TrimSpace(c.CosyVersion)
	if c.CosyVersion == "" || len(c.CosyVersion) > 32 || strings.ContainsAny(c.CosyVersion, "\r\n\t ") {
		return fmt.Errorf("invalid qoder plugin configuration: cosy_version must be a short version string")
	}
	if c.MaxTokens < 1 || c.MaxTokens > 1_000_000 {
		return fmt.Errorf("invalid qoder plugin configuration: max_tokens must be 1-1000000")
	}
	configMu.Lock()
	currentConfig = c
	configMu.Unlock()
	return nil
}
