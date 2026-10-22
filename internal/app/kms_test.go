package app

import (
	"strings"
	"testing"

	"labmill-client/internal/config"
	"labmill-client/internal/transfer"
)

func TestS3WhereKMS(t *testing.T) {
	c := config.Cluster{Name: "b", S3: &config.S3{Bucket: "bkt", SSE: "aws:kms", KMSKeyID: "alias/lab"}}
	r, err := s3Remote(c, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if r.SSE != "aws:kms" || r.KMSKeyID != "alias/lab" {
		t.Errorf("remote = %+v", r)
	}
	if got := s3Where(r); !strings.Contains(got, "encrypted with KMS") {
		t.Errorf("s3Where = %q", got)
	}
	if got := s3Where(transfer.S3Remote{Bucket: "bkt"}); strings.Contains(got, "encrypted") {
		t.Errorf("s3Where = %q", got)
	}
}
