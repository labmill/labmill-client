// Package sites holds the site profiles embedded in the Labmill binary, one
// directory per institution, named by its internet domain (for example
// "example.edu"). Each directory is shaped as a Claude Code skill
// (SKILL.md plus References/*.md: the institution's specifics that the
// generic skills in skills/ leave out) and carries a machine-readable
// Site.yaml that Labmill reads for hosts, URLs and policy. Everything specific
// to one cluster sits in a cluster folder, <domain>/<cluster>/, with a
// Cluster.yaml (the clusters.yaml template plus cluster hints) and its own
// References/*.md. internal/site loads them; README.md explains how Claude
// Code users install a site skill and docs/dev/sites.md how to add a site.
package sites

import "embed"

// FS contains every site's Site.yaml, SKILL.md and References/*.md, and every
// cluster folder's Cluster.yaml and References/*.md, rooted at the domain
// directories (e.g. "example.edu/Site.yaml",
// "example.edu/bigcluster/Cluster.yaml"). QUESTIONS.md, Evals/ and
// Diagrams/ are not embedded. Every pattern must match a file even in a
// build without the private profiles: generic/_cluster-template provides
// the cluster-folder ones (Labmill ignores folders starting with '_').
//
//go:embed */Site.yaml */SKILL.md */References/*.md */*/Cluster.yaml */*/References/*.md
var FS embed.FS
