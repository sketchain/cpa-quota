// Package plugin implements the CLIProxyAPI v8 plugin RPC contract of
// cpa-quota: registration, usage.handle and the plugin resource routes.
//
// The host calls each method synchronously through the C ABI with a JSON
// request and expects a JSON envelope back. The wire types below follow
// CLIProxyAPI's sdk/pluginapi and internal/pluginhost schemas, whose structs
// without JSON tags travel under their Go field names.
package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// Plugin identity. CLIProxyAPI derives the plugin ID from the library's
// file name, so the release ships cpa-quota.<so|dylib|dll>.
const (
	PluginID   = "cpa-quota"
	PluginName = "CPA Quota"
	Version    = "0.1.0"
	Author     = "sketchain"
	Repository = "https://github.com/sketchain/cpa-quota"
)

// schemaVersion is the plugin RPC contract version declared at registration.
// Version 6 returns management and resource JSON bodies unescaped; nothing
// this plugin uses changed after it.
const schemaVersion = 6

// RPC methods the plugin answers.
const (
	MethodPluginRegister     = "plugin.register"
	MethodPluginReconfigure  = "plugin.reconfigure"
	MethodPluginQuiesce      = "plugin.quiesce"
	MethodPluginShutdown     = "plugin.shutdown"
	MethodUsageHandle        = "usage.handle"
	MethodManagementRegister = "management.register"
	MethodManagementHandle   = "management.handle"

	methodHostLog = "host.log"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

// LifecycleRequest is the request of plugin.register and plugin.reconfigure:
// the plugin's block of plugins.configs as YAML.
type LifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32       `json:"schema_version"`
	Metadata      metadata     `json:"metadata"`
	Capabilities  capabilities `json:"capabilities"`
}

type metadata struct {
	Name             string
	Version          string
	Author           string
	GitHubRepository string
	Logo             string
	ConfigFields     []configField
}

type configField struct {
	Name        string
	Type        string
	Default     any      `json:",omitempty"`
	EnumValues  []string `json:",omitempty"`
	Description string
}

type capabilities struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
}

// UsageRecord is what usage.handle receives for each model execution.
type UsageRecord struct {
	RequestID            string
	TraceID              string
	Provider             string
	ExecutorType         string
	Model                string
	Alias                string
	AuthID               string
	AuthIndex            string
	AuthType             string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseModel        string
	Stream               bool
	RequestedAt          time.Time
	Latency              time.Duration
	Failed               bool
	Failure              UsageFailure
	Detail               UsageDetail
	ResponseHeaders      http.Header
}

// UsageFailure describes an upstream or executor failure.
type UsageFailure struct {
	StatusCode int
	Body       string
}

// UsageDetail holds the token counts.
type UsageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

// ManagementRegistrationRequest carries the plugin's route prefixes.
type ManagementRegistrationRequest struct {
	BasePath         string
	ResourceBasePath string
}

type resourceRoute struct {
	Path        string
	Menu        string `json:",omitempty"`
	Description string `json:",omitempty"`
}

type managementRegistration struct {
	Resources []resourceRoute `json:"resources,omitempty"`
}

// ManagementRequest is a request to one of the plugin's routes.
type ManagementRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Query   url.Values
	Body    []byte
}

// ManagementResponse answers a ManagementRequest.
type ManagementResponse struct {
	StatusCode int         `json:",omitempty"`
	Headers    http.Header `json:",omitempty"`
	Body       []byte      `json:",omitempty"`
}
