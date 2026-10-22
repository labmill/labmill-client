package site

// RawYAML returns a copy of the Site.yaml bytes the profile was parsed
// from, or nil for a profile built without a file (the generic fallback). The
// knowledge index (internal/knowledge) renders it for the model.
func (s Site) RawYAML() []byte { return append([]byte(nil), s.rawYAML...) }

// RawYAML returns a copy of the Cluster.yaml bytes the folder was parsed
// from, or nil when it has none.
func (c ClusterFolder) RawYAML() []byte { return append([]byte(nil), c.rawYAML...) }
