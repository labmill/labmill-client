package transfer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awss3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	_ "github.com/rclone/rclone/backend/s3" // registers the s3 backend
	rfs "github.com/rclone/rclone/fs"

	"labmill-client/internal/store"
)

// S3Remote is a location in S3 (s3://Bucket/Prefix/Dir).
//
// Credentials are never part of it: with an AWS profile (Profile, or the
// SDK's default chain when empty) they come from the AWS SDK's own sources:
// ~/.aws/config and credentials, SSO, AWS_* environment variables, instance
// roles. Region empty means the profile's region (or AWS_REGION). Endpoint
// is an optional URL for S3-compatible storage.
type S3Remote struct {
	Bucket, Prefix, Dir, Region, Profile, Endpoint string
	// SSE is "", "AES256" or "aws:kms"; KMSKeyID is an optional KMS key
	// (ID, ARN or alias) used with "aws:kms". With SSE-KMS the ETag is not
	// an MD5, so rclone skips the ETag comparison; integrity then rests on
	// the Content-MD5 header S3 verifies server-side, plus Labmill's size check.
	SSE, KMSKeyID string
}

// String returns "s3://bucket/prefix/dir" (shown in approvals). Empty prefix
// and dir parts are left out.
func (r S3Remote) String() string {
	parts := []string{r.Bucket}
	for _, p := range []string{r.Prefix, r.Dir} {
		if p = strings.Trim(p, "/"); p != "" {
			parts = append(parts, p)
		}
	}
	return "s3://" + strings.Join(parts, "/")
}

// UploadS3 has the same contract as Upload with an S3 destination: only
// manifest entries are written, each re-checked against the manifest first,
// and all keys are below s3://Bucket/Prefix/Dir/. Labmill never creates buckets
// and never lists or touches anything outside that prefix.
//
// What is verified: the real integrity check is the Content-MD5 header that
// S3 verifies server-side (single-part uploads, and each part of a multipart
// upload); a corrupted body is rejected by S3. rclone also compares the
// returned ETag with the local MD5 (or the multipart ETag, use_multipart_etag),
// except under SSE-KMS, where the ETag isn't an MD5: with SSE set no comparison
// happens, and for bucket-default KMS encryption (unknown to rclone) the first
// attempt fails and UploadS3 repeats once with the ETag comparisons off.
// The md5chksum metadata rclone writes is its copy of the local MD5, so it is
// not an independent check. UploadS3 also compares each object's size with the
// manifest. A destination whose MD5 can't be read back (e.g. a store that
// doesn't keep it) is verified by size only, as the SFTP path is. Re-running skips
// objects already present with the same size and modification time.
func UploadS3(ctx context.Context, root string, entries []store.ManifestEntry, dst S3Remote, progress func(Progress)) error {
	rootPath, err := dst.rootPath()
	if err != nil {
		return err
	}
	ctx, done, err := rcloneContext(ctx)
	if err != nil {
		return err
	}
	defer done()
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	fsrc, err := newLocalFs(ctx, absRoot)
	if err != nil {
		return err
	}
	attempt := func(ctx context.Context, lenient bool) error {
		fdst, err := newS3FsLenient(ctx, dst, rootPath, lenient)
		if err != nil {
			return err
		}
		defer shutdownFs(fdst)
		return uploadEntries(ctx, absRoot, fsrc, fdst, entries, nil, progress)
	}
	err = attempt(ctx, false)
	if isETagMismatch(err) {
		// Bucket default encryption (SSE-KMS) that Labmill wasn't told about:
		// see lenientCtx. Run again without rclone's ETag-as-MD5 comparison
		// (S3's Content-MD5 check on each request and part stays on).
		err = attempt(lenientCtx(ctx), true)
	}
	if err != nil {
		return dst.explain(err)
	}
	return nil
}

// isETagMismatch reports whether err is rclone's post-copy MD5/ETag
// comparison failing. On a bucket whose default encryption is SSE-KMS the
// ETag is a 32-hex string that is not an MD5, and rclone (which only knows
// that when server_side_encryption is set) calls the upload corrupted.
func isETagMismatch(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "md5 hashes differ") || strings.Contains(msg, "multipart upload corrupted")
}

// lenientCtx returns ctx with rclone's checksum comparison switched off.
// What still protects the data: S3 verifies the Content-MD5 header rclone
// sends (per part for multipart uploads), and Labmill compares sizes.
func lenientCtx(ctx context.Context) context.Context {
	ctx, ci := rfs.AddConfig(ctx)
	ci.IgnoreChecksum = true
	return ctx
}

// DownloadS3 has the same contract as Download with an S3 source: paths are
// relative to s3://Bucket/Prefix/Dir/ and nothing outside that prefix is read
// or listed. Local files are never replaced, remote enumeration is capped,
// local names are checked for the platform, and written sizes are verified
// (rclone also verifies the MD5 where the object has one). Objects in a
// bucket with default SSE-KMS encryption have an ETag that isn't an MD5, so
// rclone calls the download corrupted; when HeadObject confirms the failing
// object is KMS (or SSE-C) encrypted, DownloadS3 repeats once telling rclone
// so, and it then checks the md5chksum metadata where present instead. A
// mismatch on any other object is returned as an error, and the download
// never ignores checksums.
func DownloadS3(ctx context.Context, src S3Remote, paths []string, localDir string, progress func(Progress)) ([]string, error) {
	rels, err := downloadRels(paths)
	if err != nil {
		return nil, err
	}
	rootPath, err := src.rootPath()
	if err != nil {
		return nil, err
	}
	ctx, done, err := rcloneContext(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		return nil, err
	}
	fdst, err := newLocalFs(ctx, localDir)
	if err != nil {
		return nil, err
	}
	attempt := func(ctx context.Context, src S3Remote) ([]string, error) {
		fsrc, err := newS3Fs(ctx, src, rootPath)
		if err != nil {
			return nil, err
		}
		defer shutdownFs(fsrc)
		subFs := func(rel string) (rfs.Fs, error) {
			return newS3Fs(ctx, src, path.Join(rootPath, rel))
		}
		return downloadFiles(ctx, fsrc, subFs, fdst, rels, localDir, progress)
	}
	written, err := attempt(ctx, src)
	if rel, ok := etagMismatchKey(err); ok && src.SSE == "" && src.objectIsKMS(ctx, rootPath, rel) {
		// The object is SSE-KMS encrypted (bucket default), so its ETag is
		// not an MD5 and the mismatch is not corruption. Tell rclone, which
		// then compares the md5chksum metadata where the object has one.
		// Any other mismatch is returned as the corruption it may be.
		kms := src
		kms.SSE = "aws:kms"
		written, err = attempt(ctx, kms)
	}
	if err != nil {
		return written, src.explain(err)
	}
	return written, nil
}

// etagMismatchKey returns the path (relative to the S3 folder) of the file
// whose download failed rclone's MD5 comparison.
func etagMismatchKey(err error) (rel string, ok bool) {
	if err == nil || !isETagMismatch(err) {
		return "", false
	}
	msg := err.Error()
	const pre, mid = "download ", ": corrupted on transfer"
	i := strings.Index(msg, mid)
	if !strings.HasPrefix(msg, pre) || i < len(pre) {
		return "", false
	}
	return msg[len(pre):i], true
}

// objectIsKMS asks S3 (HeadObject, which needs only s3:GetObject) whether
// rel below rootPath is encrypted with KMS or a customer key, i.e. whether
// its ETag can't be expected to be an MD5. Any error means "don't know" and
// gives false.
func (r S3Remote) objectIsKMS(ctx context.Context, rootPath, rel string) bool {
	opts := []func(*awsconfig.LoadOptions) error{}
	if r.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(r.Profile))
	}
	if region := r.resolveRegion(ctx); region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return false
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		if r.Endpoint != "" {
			o.BaseEndpoint = &r.Endpoint
			o.UsePathStyle = true
		}
	})
	key := path.Join(strings.TrimPrefix(strings.TrimPrefix(rootPath, r.Bucket), "/"), rel)
	out, err := client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: &r.Bucket, Key: &key})
	if err != nil {
		return false
	}
	switch out.ServerSideEncryption {
	case awss3types.ServerSideEncryptionAwsKms, awss3types.ServerSideEncryptionAwsKmsDsse:
		return true
	}
	return out.SSECustomerAlgorithm != nil && *out.SSECustomerAlgorithm != ""
}

// rootPath validates r and returns the rclone root "bucket/prefix/dir".
func (r S3Remote) rootPath() (string, error) {
	if strings.TrimSpace(r.Bucket) == "" {
		return "", errors.New("No S3 bucket is set for this cluster.")
	}
	if strings.ContainsAny(r.Bucket, "/ ") {
		return "", fmt.Errorf("%q is not a valid S3 bucket name.", r.Bucket)
	}
	parts := []string{r.Bucket}
	for _, p := range []string{r.Prefix, r.Dir} {
		p = strings.Trim(p, "/")
		if p == "" {
			continue
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." || seg == "." || seg == "" {
				return "", fmt.Errorf("The S3 folder %q is not allowed (no '..', '.' or empty parts).", p)
			}
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "/"), nil
}

// newS3Fs builds rclone's s3 backend in-process: no config file, no RCLONE_*
// environment (see optionDefaults) and never an access key. Credentials come
// from the AWS SDK default chain (env_auth) for r.Profile. The AWS provider
// is used unless an endpoint is given ("Other", path-style addressing).
// no_check_bucket stops rclone from creating a missing bucket.
func newS3Fs(ctx context.Context, r S3Remote, root string) (rfs.Fs, error) {
	return newS3FsLenient(ctx, r, root, false)
}

// newS3FsLenient is newS3Fs; with lenient set (uploads only) rclone also
// stops comparing the multipart ETag (see lenientCtx).
func newS3FsLenient(ctx context.Context, r S3Remote, root string, lenient bool) (rfs.Fs, error) {
	region := r.resolveRegion(ctx)
	ov := map[string]string{
		"provider":        "AWS",
		"env_auth":        "true",
		"region":          region,
		"profile":         r.Profile,
		"no_check_bucket": "true",
	}
	for k, v := range r.sseOptions() {
		ov[k] = v
	}
	if lenient {
		ov["use_multipart_etag"] = "false"
	}
	if r.Endpoint != "" {
		ov["provider"] = "Other"
		ov["endpoint"] = r.Endpoint
	}
	f, err := newBackendFs(ctx, "s3", root, ov)
	if errors.Is(err, rfs.ErrorIsFile) {
		return nil, fmt.Errorf("The destination s3://%s is a file in the bucket.", root)
	}
	if err != nil {
		return nil, r.explain(fmt.Errorf("s3://%s: %w", root, err))
	}
	return f, nil
}

// sseOptions returns rclone's s3 options for the requested encryption.
func (r S3Remote) sseOptions() map[string]string {
	ov := map[string]string{}
	if r.SSE != "" {
		ov["server_side_encryption"] = r.SSE
		if r.SSE == "aws:kms" && r.KMSKeyID != "" {
			ov["sse_kms_key_id"] = r.KMSKeyID
		}
	}
	return ov
}

// resolveRegion returns Region, or, with neither a region nor an endpoint set,
// the region of the AWS profile / environment (rclone would otherwise force
// us-east-1 over the profile's region). It does no network access.
func (r S3Remote) resolveRegion(ctx context.Context) string {
	if r.Region != "" || r.Endpoint != "" {
		return r.Region
	}
	opts := []func(*awsconfig.LoadOptions) error{}
	if r.Profile != "" {
		opts = append(opts, awsconfig.WithSharedConfigProfile(r.Profile))
	}
	if cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...); err == nil {
		return cfg.Region
	}
	return ""
}

// explain rewrites the common S3 failures in plain language and otherwise
// returns err unchanged.
func (r S3Remote) explain(err error) error {
	msg := err.Error()
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(msg, s) {
				return true
			}
		}
		return false
	}
	profile := r.Profile
	if profile == "" {
		profile = "your default AWS profile"
	} else {
		profile = "AWS profile " + profile
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case has("NoSuchBucket", "directory not found"):
		return fmt.Errorf("The S3 bucket %q doesn't exist, or %s can't see it (Labmill never creates buckets): %w", r.Bucket, profile, err)
	case has("couldn't load configuration", "failed to refresh cached credentials", "no EC2 IMDS", "NoCredentialProviders", "InvalidAccessKeyId", "ExpiredToken", "token has expired", "SSO session"):
		return fmt.Errorf("I couldn't sign in to AWS with %s. Run `aws configure` or `aws sso login` and try again: %w", profile, err)
	case has("AccessDenied", "SignatureDoesNotMatch", "Forbidden"):
		return fmt.Errorf("AWS refused access to bucket %q with %s: %w", r.Bucket, profile, err)
	}
	return err
}
