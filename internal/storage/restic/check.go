package restic

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// LocationEnv are the environment variables a Location may set: the
// credentials and region of an s3 repository (see S3Repository.Location).
var LocationEnv = []string{
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN", "AWS_DEFAULT_REGION", "AWS_REGION",
}

// windowsAbs matches a Windows path with a drive letter (C:\ or C:/).
var windowsAbs = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// Check reports an error unless l asks restic for nothing beyond what
// gos3's own repositories need: an s3 repository ("s3:…") or a local one
// (a path, or "local:…"), only "s3.*" extended options, and only the
// LocationEnv variables. Restic runs programs named by other backends
// (sftp's ssh, rclone), by options such as sftp.command or
// rclone.program and by variables such as RESTIC_PASSWORD_COMMAND, so a
// Location that does not pass could run commands on the machine running
// restic. The CLI checks the Locations a server sends; Runtime checks
// every Location before running restic.
func (l Location) Check() error {
	if err := checkRepository(l.Repository); err != nil {
		return err
	}
	for _, o := range l.Options {
		name, _, _ := strings.Cut(o, "=")
		if !strings.HasPrefix(name, "s3.") || strings.ContainsFunc(name, isSpaceOrControl) {
			return fmt.Errorf("restic: option %q is not allowed (only s3.* options are)", clean(name, 80))
		}
	}
	for _, e := range l.Env {
		name, _, ok := strings.Cut(e, "=")
		if !ok || !slices.Contains(LocationEnv, name) {
			return fmt.Errorf("restic: environment variable %q is not allowed (only %s are)", clean(name, 80), strings.Join(LocationEnv, ", "))
		}
	}
	return nil
}

// checkRepository accepts an s3 repository, "local:…" and paths. Restic
// takes the text before the first ':' as the backend's name and falls back
// to a local path when no backend has that name, so a repository without a
// ':' or with a drive letter (C:\…) is a path; any other "name:" might
// name another backend and is refused.
func checkRepository(repo string) error {
	switch {
	case repo == "":
		return fmt.Errorf("restic: no repository")
	case strings.ContainsFunc(repo, isControl):
		return fmt.Errorf("restic: the repository %q contains control characters", clean(repo, 120))
	case strings.HasPrefix(repo, "s3:"), strings.HasPrefix(repo, "local:"),
		!strings.Contains(repo, ":"), windowsAbs.MatchString(repo):
		return nil
	}
	return fmt.Errorf("restic: repository %q is not allowed (only s3: and local repositories are)", clean(repo, 120))
}

func isControl(r rune) bool { return r < ' ' || r == 0x7f }

func isSpaceOrControl(r rune) bool { return r <= ' ' || r == 0x7f }
