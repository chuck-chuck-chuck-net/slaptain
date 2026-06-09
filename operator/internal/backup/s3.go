/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package backup implements the S3 transfer primitives used by the backup and
// restore Jobs (ADR-014). It uses aws-sdk-go-v2 (deliberately not minio-go) and
// works against AWS S3 and any S3-compatible store (MinIO, Ceph RGW).
package backup

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha1" //nolint:gosec // SSHA verification requires SHA-1 (the LDAP scheme)
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Config addresses an S3 (or S3-compatible) bucket. Credentials are NOT
// carried here: they are read from the standard AWS environment
// (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, optionally AWS_SESSION_TOKEN) via
// the SDK's default credential chain. The backup/restore Job maps the
// credentialsSecretName keys onto those env vars, so secrets never appear in
// argv.
type S3Config struct {
	// Bucket is the target S3 bucket.
	Bucket string
	// Endpoint is the S3 endpoint URL. Empty means AWS S3; set it for an
	// S3-compatible store (enables path-style addressing).
	Endpoint string
	// Region is the S3 region. Some S3-compatible stores ignore it.
	Region string
	// InsecureTLS disables TLS verification against the endpoint (test-only).
	InsecureTLS bool
	// AccessKeyID / SecretAccessKey supply static credentials. When both are
	// empty the SDK's default credential chain (AWS_ACCESS_KEY_ID etc.) is used
	// instead — that is the path the backup/restore Jobs take. The operator's
	// inline retention deletes set these explicitly from the credentials Secret.
	AccessKeyID     string
	SecretAccessKey string
}

// newClient builds an S3 client honoring a custom endpoint (path-style for
// S3-compatible stores) and optional insecure TLS.
func newClient(ctx context.Context, cfg S3Config) (*s3.Client, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}
	if cfg.InsecureTLS {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // opt-in test-only flag
			},
		}))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // MinIO/Ceph need path-style addressing
		}
	}), nil
}

// MetaReplHashKey is the S3 object user-metadata key under which a backup stores
// the {SSHA} userPassword of cn=replication, so a restore can verify the
// provided replication-password against the backup without scanning the LDIF
// (ADR-014 amendment). Empty/absent when the source had no cn=replication.
const MetaReplHashKey = "replication-pw-hash"

// Upload streams localPath to s3://<bucket>/<key>, using multipart upload for
// large objects, attaching the given user-metadata. Returns the uploaded size.
func Upload(ctx context.Context, cfg S3Config, key, localPath string, metadata map[string]string) (int64, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", localPath, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", localPath, err)
	}

	client, err := newClient(ctx, cfg)
	if err != nil {
		return 0, err
	}

	// transfermanager.UploadObject buffers large files into parts and uploads
	// them in parallel (multipart).
	if _, err := transfermanager.New(client).UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:   aws.String(cfg.Bucket),
		Key:      aws.String(key),
		Body:     f,
		Metadata: metadata,
	}); err != nil {
		return 0, fmt.Errorf("upload to s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	return fi.Size(), nil
}

// Download streams s3://<bucket>/<key> to outPath.
func Download(ctx context.Context, cfg S3Config, key, outPath string) error {
	client, err := newClient(ctx, cfg)
	if err != nil {
		return err
	}

	resp, err := transfermanager.New(client).GetObject(ctx, &transfermanager.GetObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("download s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	if c, ok := resp.Body.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}

	// Close explicitly (not deferred) so the flush error is surfaced.
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("write %s: %w", outPath, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", outPath, closeErr)
	}
	return nil
}

// Preflight validates that a backup object is restorable BEFORE any destructive
// restore step, so a bad/unreachable/wrong backup fails with neither downtime
// nor data loss (ADR-014 amendment). A single GetObject yields both the object
// metadata (verified first) and the body (sampled for validity):
//
//  1. If replPassword != "" and the object carries the replication-pw-hash
//     metadata (set by a slaptain backup), SSHA-verify it. Default-deny: a
//     mismatch OR an unverifiable scheme fails the restore. The cn=replication
//     password is part of the restored data — we do NOT rewrite it (that would
//     be a hidden rotation), so the restore cluster's credentials Secret must
//     carry the source's replication-password. The hash is exact metadata, not
//     a scan, so there is no large-backup blind spot. (A direct-S3 legacy dump
//     has no such metadata → not verified; provide matching creds.)
//  2. Confirm the object decompresses and contains the suffix entry
//     (`dn: <suffix>`) — slapcat emits the suffix entry first, so a short read
//     suffices.
func Preflight(ctx context.Context, cfg S3Config, key, suffix, replPassword string) error {
	client, err := newClient(ctx, cfg)
	if err != nil {
		return err
	}
	resp, err := transfermanager.New(client).GetObject(ctx, &transfermanager.GetObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("download s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	if c, ok := resp.Body.(io.Closer); ok {
		defer func() { _ = c.Close() }()
	}

	// 1. Replication password — exact verification from object metadata.
	if replPassword != "" {
		if replHash := metaValue(resp.Metadata, MetaReplHashKey); replHash != "" && !sshaMatches(replHash, replPassword) {
			return fmt.Errorf("could not verify the replication-password against backup s3://%s/%s "+
				"(password mismatch, or unsupported hash scheme) — a restored replicated database keeps the "+
				"backup's password, so provide the source cluster's replication-password", cfg.Bucket, key)
		}
	}

	// 2. Validity — the object decompresses and its first entry is the suffix.
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("backup s3://%s/%s is not valid gzip: %w", cfg.Bucket, key, err)
	}
	defer func() { _ = gz.Close() }()

	want := "dn: " + suffix
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	const maxLines = 1000 // the suffix entry is slapcat's first entry
	for lines := 0; sc.Scan() && lines < maxLines; lines++ {
		if strings.EqualFold(strings.TrimSpace(sc.Text()), want) {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read backup s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	return fmt.Errorf("backup s3://%s/%s does not contain suffix entry %q (wrong or corrupt backup)", cfg.Bucket, key, suffix)
}

// ldifValue extracts attr's value from an LDIF line, handling base64 (`attr:: …`)
// and plain (`attr: …`) forms.
func ldifValue(line, attr string) (string, bool) {
	switch {
	case strings.HasPrefix(line, attr+":: "):
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len(attr)+3:]))
		if err != nil {
			return "", false
		}
		return string(dec), true
	case strings.HasPrefix(line, attr+": "):
		return strings.TrimSpace(line[len(attr)+2:]), true
	default:
		return "", false
	}
}

// metaValue looks up an S3 user-metadata value case-insensitively (S3 stores
// metadata keys lowercased, and SDKs vary in how they return the case).
func metaValue(meta map[string]string, key string) string {
	for k, v := range meta {
		if strings.EqualFold(k, key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ReplicationHashFromLDIF reads a small LDIF file — a targeted slapcat of
// cn=replication produced by the backup Job — and returns its userPassword
// value, or "" if the file is absent or has no cn=replication entry. The backup
// uploader stamps this onto the object's replication-pw-hash metadata.
func ReplicationHashFromLDIF(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	// Unfold LDIF continuation lines, then find userPassword.
	logical := make([]string, 0, strings.Count(string(data), "\n")+1)
	for raw := range strings.SplitSeq(string(data), "\n") {
		raw = strings.TrimRight(raw, "\r")
		if strings.HasPrefix(raw, " ") && len(logical) > 0 {
			logical[len(logical)-1] += raw[1:]
			continue
		}
		logical = append(logical, raw)
	}
	for _, line := range logical {
		if v, ok := ldifValue(line, "userPassword"); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", nil
}

// sshaMatches reports whether a {SSHA} userPassword value matches password.
// Unknown schemes return false-without-erroring at the call site (we only verify
// the {SSHA} the operator itself writes; other schemes are left unverified).
func sshaMatches(stored, password string) bool {
	const scheme = "{SSHA}"
	if !strings.HasPrefix(stored, scheme) {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(stored[len(scheme):])
	if err != nil || len(raw) <= sha1.Size {
		return false
	}
	digest, salt := raw[:sha1.Size], raw[sha1.Size:]
	h := sha1.New() //nolint:gosec // SSHA is SHA-1 by definition
	h.Write([]byte(password))
	h.Write(salt)
	return subtle.ConstantTimeCompare(h.Sum(nil), digest) == 1
}

// Delete removes s3://<bucket>/<key>. Used by retention pruning. It is a small
// control-plane call (not a data transfer), so the operator runs it inline.
func Delete(ctx context.Context, cfg S3Config, key string) error {
	client, err := newClient(ctx, cfg)
	if err != nil {
		return err
	}
	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("delete s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	return nil
}
