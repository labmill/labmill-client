package transfer

import "testing"

func TestRcloneCommand(t *testing.T) {
	sftp := Remote{SSHCommand: []string{"/usr/bin/labmill", "ssh-exec", "hpcx"}, Dir: "labmill/job1"}
	sftpSp := Remote{SSHCommand: []string{`C:\Program Files\labmill\labmill.exe`, "ssh-exec", "hpcx"}, Dir: "/storage/my group/it's"}
	s3 := S3Remote{Bucket: "bkt", Prefix: "pre", Dir: "job1", Profile: "lab", Region: "us-west-2", SSE: "aws:kms", KMSKeyID: "alias/k"}
	s3o := S3Remote{Bucket: "bkt", Dir: "job1", Endpoint: "https://s3.example.org:9000"}
	files := []string{"a.csv", "sub/b.txt"}
	const ssh = "--sftp-ssh '/usr/bin/labmill ssh-exec hpcx' --sftp-shell-type none --sftp-disable-hashcheck"
	tests := []struct {
		name, goos, want string
		got              func(goos string) string
	}{
		{"sftp upload linux", "linux",
			"rclone-lib: copy --files-from-raw files.txt --no-traverse " + ssh + " /data/in :sftp:labmill/job1  # files.txt lists the 2 approved files",
			func(g string) string { return sftp.rcloneCommand(g, "/data/in", files, true) }},
		{"sftp download linux", "linux",
			"rclone-lib: copy " + ssh + " :sftp:labmill/job1/out/a.csv /res\n" +
				"rclone-lib: copy " + ssh + " :sftp:labmill/job1/log.txt /res",
			func(g string) string { return sftp.rcloneCommand(g, "/res", []string{"out/a.csv", "log.txt"}, false) }},
		{"sftp spaces and quotes linux", "linux",
			"rclone-lib: copy --files-from-raw files.txt --no-traverse --sftp-ssh '\"C:\\Program Files\\labmill\\labmill.exe\" ssh-exec hpcx' --sftp-shell-type none --sftp-disable-hashcheck '/my data' ':sftp:/storage/my group/it'\\''s'  # files.txt lists the 2 approved files",
			func(g string) string { return sftpSp.rcloneCommand(g, "/my data", files, true) }},
		{"sftp spaces and quotes windows", "windows",
			"rclone-lib: copy --files-from-raw files.txt --no-traverse --sftp-ssh '\"C:\\Program Files\\labmill\\labmill.exe\" ssh-exec hpcx' --sftp-shell-type none --sftp-disable-hashcheck 'C:\\my data' ':sftp:/storage/my group/it''s'  # files.txt lists the 2 approved files",
			func(g string) string { return sftpSp.rcloneCommand(g, `C:\my data`, files, true) }},
		{"s3 upload", "linux",
			"rclone-lib: copy --files-from-raw files.txt --no-traverse --s3-no-check-bucket /data/in ':s3,provider=AWS,env_auth=true,profile=lab,region=us-west-2,server_side_encryption='\\''aws:kms'\\'',sse_kms_key_id=alias/k:bkt/pre/job1'  # files.txt lists the 2 approved files",
			func(g string) string { return s3.rcloneCommand(g, "/data/in", files, true) }},
		{"s3 download endpoint windows", "windows",
			"rclone-lib: copy --s3-no-check-bucket ':s3,provider=Other,env_auth=true,endpoint=''https://s3.example.org:9000'':bkt/job1/out' 'C:\\res dir'",
			func(g string) string { return s3o.rcloneCommand(g, `C:\res dir`, []string{"out"}, false) }},
		{"s3 plain windows is quoted", "windows",
			"rclone-lib: copy --s3-no-check-bucket ':s3,provider=AWS,env_auth=true,region=us-west-2:bkt/job1/f' 'C:\\res'",
			func(g string) string {
				return S3Remote{Bucket: "bkt", Dir: "job1", Region: "us-west-2"}.rcloneCommand(g, `C:\res`, []string{"f"}, false)
			}},
		{"smart quote windows", "windows",
			"rclone-lib: copy --s3-no-check-bucket ':s3,provider=AWS,env_auth=true,region=us-west-2:bkt/it\u2019\u2019s' 'C:\\res'",
			func(g string) string {
				return S3Remote{Bucket: "bkt", Region: "us-west-2"}.rcloneCommand(g, `C:\res`, []string{"it\u2019s"}, false)
			}},
		{"comma and at windows", "windows",
			"rclone-lib: copy --sftp-ssh 'labmill ssh-exec a' --sftp-shell-type none --sftp-disable-hashcheck ':sftp:a,b/f' '@x'",
			func(g string) string {
				return Remote{SSHCommand: []string{"labmill", "ssh-exec", "a"}, Dir: "a,b"}.rcloneCommand(g, "@x", []string{"f"}, false)
			}},
		{"newline in path", "linux",
			"rclone-lib: copy --sftp-ssh 'labmill ssh-exec a' --sftp-shell-type none --sftp-disable-hashcheck ':sftp:d/x?rclone-lib: evil' /r",
			func(g string) string {
				return Remote{SSHCommand: []string{"labmill", "ssh-exec", "a"}, Dir: "d"}.rcloneCommand(g, "/r", []string{"x\nrclone-lib: evil"}, false)
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.got(tt.goos); got != tt.want {
				t.Errorf("got\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
}
