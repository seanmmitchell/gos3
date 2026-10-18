package restic

import (
	"strings"
	"testing"
)

// Location.Check accepts gos3's own repositories and refuses whatever
// could make restic (or a shell evaluating `gos3 restic env`) run a
// program.
func TestLocationCheck(t *testing.T) {
	s3 := S3Repository{Endpoint: "https://s3.example.com", Bucket: "b", Path: "p", Region: "eu-1",
		AccessKey: "AK", SecretKey: "SK", SessionToken: "ST"}.Location("pw", "")
	for _, loc := range []Location{
		s3,
		{Repository: "/srv/gos3/.gos3/restic/abc", Password: "pw"},
		{Repository: `C:\gos3\.gos3\restic\abc`, Password: "pw"},
		{Repository: "C:/gos3/restic", Password: "pw"},
		{Repository: `data\.gos3\restic\abc`, Password: "pw"},
		{Repository: "local:/srv/repo", Password: "pw"},
		{Repository: "s3:http://127.0.0.1:9000/b", Env: []string{"AWS_REGION=x"}, Options: []string{"s3.connections=2"}},
	} {
		if err := loc.Check(); err != nil {
			t.Errorf("%+v refused: %v", loc, err)
		}
	}
	for _, tc := range []struct {
		loc  Location
		want string
	}{
		{Location{}, "no repository"},
		{Location{Repository: `\\attacker\share\repo`}, "network"},
		{Location{Repository: `//attacker/share/repo`}, "network"},
		{Location{Repository: `local:\\attacker\share\repo`}, "network"},
		{Location{Repository: `local://attacker/share/repo`}, "network"},
		{Location{Repository: `\\?\UNC\attacker\share`}, "network"},
		{Location{Repository: `\/attacker/share`}, "network"},
		{Location{Repository: "sftp:-oProxyCommand=id:/x"}, "repository"},
		{Location{Repository: "rclone:remote:path"}, "repository"},
		{Location{Repository: "rest:https://user:pw@host/"}, "repository"},
		{Location{Repository: "b2:bucket:path"}, "repository"},
		{Location{Repository: "/repo\nRESTIC_PASSWORD_COMMAND=id"}, "control"},
		{Location{Repository: "/repo", Options: []string{"sftp.command=sh -c id"}}, "option"},
		{Location{Repository: "/repo", Options: []string{"rclone.program=/tmp/x"}}, "option"},
		{Location{Repository: "/repo", Options: []string{"s3 .x=1"}}, "option"},
		{Location{Repository: "/repo", Env: []string{"RESTIC_PASSWORD_COMMAND=sh -c id"}}, "environment"},
		{Location{Repository: "/repo", Env: []string{"PROMPT_COMMAND=id"}}, "environment"},
		{Location{Repository: "/repo", Env: []string{"LD_PRELOAD=/tmp/x.so"}}, "environment"},
		{Location{Repository: "/repo", Env: []string{"aws_access_key_id=x"}}, "environment"},
		{Location{Repository: "/repo", Env: []string{"AWS_ACCESS_KEY_ID"}}, "environment"},
	} {
		err := tc.loc.Check()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v, want an error about %s", tc.loc, err, tc.want)
		}
	}
}

// Runtime refuses such a Location before looking for restic.
func TestCommandRefusesUnsafeLocation(t *testing.T) {
	rt := &Runtime{LookPath: func(string) (string, error) {
		t.Fatal("restic was looked up for an unsafe location")
		return "", nil
	}}
	loc := Location{Repository: "/repo", Password: "pw", Env: []string{"RESTIC_PASSWORD_COMMAND=sh -c id"}}
	if _, err := rt.Command(t.Context(), loc, "snapshots"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("Command = %v", err)
	}
}
