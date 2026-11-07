package main

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginStoreID = "omc-qoder-oauth"
const resourcePrefix = "/v0/resource/plugins/" + pluginStoreID

// publicAssetBaseURL is the published location of the same artwork the resource
// routes serve. OAuth entry logos use it because a panel reads a plugin logo as
// a document, and the store manifests are URLs too.
const publicAssetBaseURL = "https://raw.githubusercontent.com/omc-community/omc-qoder-oauth/refs/heads/main/assets/"

// pluginLogo is the official Qoder mark: the light tile with the dark glyph.
const pluginLogo = "qoder-color.svg"

//go:embed assets/qoder-color.svg
var qoderLogoColorSVG []byte

//go:embed assets/qoder.svg
var qoderLogoMonoSVG []byte

// resourceAssetURL is the public address of one embedded logo. Only the two
// names registered below are addressable, so the set of URLs the plugin can
// publish stays equal to the set of bytes it ships.
func resourceAssetURL(name string) string {
	switch name {
	case "qoder.svg", "qoder-color.svg":
		return publicAssetBaseURL + name
	default:
		return ""
	}
}

// handleManagementResource serves only deployed public bytes. It deliberately
// has no host callback, auth directory, configuration or OAuth-state dependency.
func handleManagementResource(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	var body []byte
	contentType := "image/svg+xml"
	cacheControl := "public, max-age=86400"
	switch req.Path {
	case resourcePrefix + "/qoder-color.svg":
		body = qoderLogoColorSVG
	case resourcePrefix + "/qoder.svg":
		body = qoderLogoMonoSVG
	case omcResourcePath:
		body = []byte(omcDescriptorJSON)
		contentType = "application/json; charset=utf-8"
		cacheControl = "no-cache"
	default:
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte("not found")}, nil
	}
	if req.Method != http.MethodGet {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    http.Header{"Allow": {http.MethodGet}},
			Body:       []byte("method not allowed"),
		}, nil
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {contentType},
			"Cache-Control":           {cacheControl},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'"},
		},
		Body: append([]byte(nil), body...),
	}, nil
}
