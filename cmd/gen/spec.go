package main

// Backend maps a logical key to a default host and the envvar that overrides it.
type Backend struct {
	HostDefault string `yaml:"host_default" json:"host_default"`
	HostEnv     string `yaml:"host_env" json:"host_env"`
}

// Defaults are applied to endpoints that omit the corresponding field.
type Defaults struct {
	OutputEncoding string `yaml:"output_encoding"`
	Encoding       string `yaml:"encoding"`
	Timeout        string `yaml:"timeout"`
}

// RateLimit is an optional per-endpoint qos/ratelimit/router config.
type RateLimit struct {
	MaxRate       float64 `yaml:"max_rate" json:"max_rate"`
	ClientMaxRate float64 `yaml:"client_max_rate" json:"client_max_rate"`
	Strategy      string  `yaml:"strategy" json:"strategy"`
}

// Endpoint is one route. Fields with json:"-" are consumed during normalize
// and not emitted; the rest are emitted into endpoints.json in this order.
type Endpoint struct {
	Path              string     `yaml:"path" json:"path"`
	Method            string     `yaml:"method" json:"method"`
	Backend           string     `yaml:"backend" json:"-"`
	Auth              string     `yaml:"auth" json:"auth"`
	URLPattern        string     `yaml:"url_pattern" json:"url_pattern"`
	OutputEncoding    string     `yaml:"output_encoding" json:"output_encoding"`
	Encoding          string     `yaml:"encoding" json:"encoding"`
	HostEnv           string     `yaml:"-" json:"host_env"`
	HostDefault       string     `yaml:"-" json:"host_default"`
	InputHeaders      []string   `yaml:"input_headers" json:"input_headers"`
	InputQueryStrings []string   `yaml:"input_query_strings" json:"input_query_strings"`
	Timeout           string     `yaml:"timeout" json:"timeout"`
	RateLimit         *RateLimit `yaml:"rate_limit" json:"rate_limit"`
}

// Spec is the full endpoints.yaml document.
type Spec struct {
	Backends  map[string]Backend `yaml:"backends"`
	Defaults  Defaults           `yaml:"defaults"`
	Endpoints []Endpoint         `yaml:"endpoints"`
}
