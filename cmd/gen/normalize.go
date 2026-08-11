package main

// Normalize applies defaults and resolves derived fields, returning the
// endpoints in input order ready for JSON emission.
func Normalize(spec Spec) []Endpoint {
	out := make([]Endpoint, len(spec.Endpoints))
	for i, e := range spec.Endpoints {
		if e.OutputEncoding == "" {
			e.OutputEncoding = spec.Defaults.OutputEncoding
		}
		if e.Encoding == "" {
			e.Encoding = spec.Defaults.Encoding
		}
		if e.Timeout == "" {
			e.Timeout = spec.Defaults.Timeout
		}
		if e.URLPattern == "" {
			e.URLPattern = e.Path
		}
		if b, ok := spec.Backends[e.Backend]; ok {
			e.HostEnv = b.HostEnv
			e.HostDefault = b.HostDefault
		}
		if e.InputQueryStrings == nil {
			e.InputQueryStrings = []string{}
		}
		out[i] = e
	}
	return out
}
