package transfer

import (
	"bytes"
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// liveS3Env returns the bucket settings for TestLiveS3 or skips.
func liveS3Env(t *testing.T) (bucket, kmsBucket, region string) {
	t.Helper()
	if os.Getenv("LABMILL_LIVE") != "1" || os.Getenv("LABMILL_LIVE_S3") != "1" {
		t.Skip("set LABMILL_LIVE=1 and LABMILL_LIVE_S3=1 (plus LABMILL_LIVE_S3_BUCKET) to run against real S3")
	}
	bucket = os.Getenv("LABMILL_LIVE_S3_BUCKET")
	if bucket == "" {
		t.Skip("set LABMILL_LIVE_S3_BUCKET")
	}
	region = os.Getenv("LABMILL_LIVE_S3_REGION")
	if region == "" {
		region = "us-west-2"
	}
	return bucket, os.Getenv("LABMILL_LIVE_S3_KMS_BUCKET"), region
}

// writeLiveTree writes small files plus one random file of bigMiB MiB.
func writeLiveTree(t *testing.T, dir string, bigMiB int) []string {
	t.Helper()
	rng := rand.New(rand.NewSource(7))
	sizes := map[string]int{"a.bin": 60000, "sub/b.txt": 30000, "données.csv": 1000, "empty.txt": 0}
	if bigMiB > 0 {
		sizes["big.bin"] = bigMiB << 20
	}
	var rels []string
	for rel, n := range sizes {
		data := make([]byte, n)
		rng.Read(data)
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	return rels
}

func liveRoundTrip(ctx context.Context, t *testing.T, remote S3Remote, bigMiB int) {
	t.Helper()
	src := t.TempDir()
	rels := writeLiveTree(t, src, bigMiB)
	entries, skipped, err := BuildManifest(ctx, src, rels, nil)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("manifest: %v skipped=%v", err, skipped)
	}
	var last Progress
	if err := UploadS3(ctx, src, entries, remote, func(p Progress) { last = p }); err != nil {
		t.Fatalf("UploadS3 (%s): %v", remote, err)
	}
	t.Logf("uploaded %d files, %d bytes to %s", len(entries), last.TotalBytes, remote)
	dst := t.TempDir()
	if _, err := DownloadS3(ctx, remote, rels, dst, nil); err != nil {
		t.Fatalf("DownloadS3 (%s): %v", remote, err)
	}
	for _, rel := range rels {
		a, _ := os.ReadFile(filepath.Join(src, filepath.FromSlash(rel)))
		b, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(rel)))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s differs after round trip (err %v)", rel, err)
		}
	}
	// Second run: objects already there are skipped, no error.
	if err := UploadS3(ctx, src, entries, remote, nil); err != nil {
		t.Errorf("re-upload: %v", err)
	}
}

// TestLiveS3 is an opt-in check against real S3: small and multipart
// (over 200 MiB) round trips on a plain and an SSE-KMS bucket (whose ETag
// isn't an MD5), and a wrong region setting. Run it with
//
//	LABMILL_LIVE=1 LABMILL_LIVE_S3=1 LABMILL_LIVE_S3_BUCKET=<bucket> \
//	  [LABMILL_LIVE_S3_KMS_BUCKET=<bucket> LABMILL_LIVE_S3_REGION=<region>] \
//	  [LABMILL_LIVE_S3_PROFILE=<profile>] \
//	  go test ./internal/transfer -run TestLiveS3 -v
//
// It never creates or deletes buckets or objects itself (re-runs overwrite;
// rclone may remove its own failed upload under the prefix).
func TestLiveS3(t *testing.T) {
	bucket, kms, region := liveS3Env(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	profile := os.Getenv("LABMILL_LIVE_S3_PROFILE")

	t.Run("plain", func(t *testing.T) {
		liveRoundTrip(ctx, t, S3Remote{Bucket: bucket, Prefix: "labmill-live", Dir: "plain", Region: region, Profile: profile}, 210)
	})
	if kms != "" {
		t.Run("kms-default-encryption", func(t *testing.T) {
			liveRoundTrip(ctx, t, S3Remote{Bucket: kms, Prefix: "labmill-live", Dir: "kms", Region: region, Profile: profile}, 210)
		})
		t.Run("kms-explicit-sse", func(t *testing.T) {
			liveRoundTrip(ctx, t, S3Remote{Bucket: kms, Prefix: "labmill-live", Dir: "kms-sse", Region: region, Profile: profile, SSE: "aws:kms"}, 0)
		})
	}
	t.Run("wrong-region", func(t *testing.T) {
		wrong := "us-east-1"
		if region == wrong {
			wrong = "eu-west-1"
		}
		remote := S3Remote{Bucket: bucket, Prefix: "labmill-live", Dir: "wrong-region", Region: wrong, Profile: profile}
		src := t.TempDir()
		rels := writeLiveTree(t, src, 0)
		entries, _, err := BuildManifest(ctx, src, rels, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = UploadS3(ctx, src, entries, remote, nil)
		if err != nil {
			t.Logf("wrong region error: %v", err)
			if !strings.Contains(err.Error(), region) {
				t.Errorf("error doesn't name the bucket's region %s", region)
			}
			return
		}
		t.Logf("wrong region %s was followed to %s without error", wrong, region)
	})
}
