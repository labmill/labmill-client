package rules

import (
	"path"
	"strings"
)

// JobFolderParent is the folder name job folders sit in: a job's data
// folder is <...>/labmill/<job name> (upload_data's layout).
const JobFolderParent = "labmill"

// IsJobFolder reports whether dir (absolute and clean) is a job's own data
// folder, <...>/labmill/<job>: its last element is the job's folder and the
// one before it is exactly "labmill". job, when not "", must equal that last
// element.
func IsJobFolder(dir, job string) bool {
	if !strings.HasPrefix(dir, "/") || path.Clean(dir) != dir {
		return false
	}
	parent, base := path.Split(dir)
	if base == "" || base == "." || base == ".." || (job != "" && base != job) {
		return false
	}
	return path.Base(strings.TrimRight(parent, "/")) == JobFolderParent && strings.TrimRight(parent, "/") != "/"+JobFolderParent
}
