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
	"crypto/tls"
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

// Upload streams localPath to s3://<bucket>/<key>, using multipart upload for
// large objects. Returns the uploaded size in bytes.
func Upload(ctx context.Context, cfg S3Config, key, localPath string) (int64, error) {
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
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
		Body:   f,
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

// Preflight validates that a backup object is restorable WITHOUT downloading the
// whole thing: it streams the object, gunzips, and confirms the suffix entry
// (`dn: <suffix>`) appears near the start (slapcat emits the suffix entry first).
// Run before any destructive restore step so a bad/unreachable/wrong backup
// fails with neither downtime nor data loss (ADR-014 amendment). The read is
// bounded — it stops as soon as the suffix entry is found.
func Preflight(ctx context.Context, cfg S3Config, key, suffix string) error {
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

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("backup s3://%s/%s is not valid gzip: %w", cfg.Bucket, key, err)
	}
	defer func() { _ = gz.Close() }()

	want := "dn: " + suffix
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	const maxLines = 10000 // the suffix entry is at the top; bound the read
	for lines := 0; sc.Scan() && lines < maxLines; lines++ {
		if strings.EqualFold(strings.TrimSpace(sc.Text()), want) {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read backup s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	return fmt.Errorf("backup s3://%s/%s does not contain suffix entry %q (wrong or corrupt backup)", cfg.Bucket, key, want)
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
