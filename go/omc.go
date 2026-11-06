package main

import _ "embed"

const omcResourcePath = "/v0/resource/plugins/" + pluginStoreID + "/omc.json"

// omcDescriptorJSON is public, build-bound documentation, never runtime state.
// Release packaging copies this same file without reserializing it.
//
//go:embed assets/omc.json
var omcDescriptorJSON string
