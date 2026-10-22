package app

import "strings"

// evalLinks resolves p to the real path the privacy and folder checks work
// on (CheckFolder, insideRoots, the workspace and results folders). It is a
// variable so tests can swap the resolver; the default is resolveLinks:
// filepath.EvalSymlinks everywhere, plus on Windows the final path of the
// handle, because since Go 1.23 EvalSymlinks no longer resolves junctions
// (mount points), so a junction inside the home folder that points at a
// private place would pass for an ordinary folder.
var evalLinks = resolveLinks

// stripExtendedPrefix turns the extended-length form GetFinalPathNameByHandle
// returns into the ordinary path: `\\?\C:\x` becomes `C:\x` and
// `\\?\UNC\server\share\x` becomes `\\server\share\x`. Other paths
// (including device paths such as `\\?\Volume{...}\`) are returned as they
// are.
func stripExtendedPrefix(p string) string {
	const ext = `\\?\`
	if !strings.HasPrefix(p, ext) {
		return p
	}
	rest := p[len(ext):]
	if len(rest) >= 4 && strings.EqualFold(rest[:4], `UNC\`) {
		return `\\` + rest[4:]
	}
	if len(rest) >= 2 && rest[1] == ':' && (rest[0] >= 'A' && rest[0] <= 'Z' || rest[0] >= 'a' && rest[0] <= 'z') {
		return rest
	}
	return p
}
