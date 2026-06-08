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
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
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
}

// newClient builds an S3 client honoring a custom endpoint (path-style for
// S3-compatible stores) and optional insecure TLS.
func newClient(ctx context.Context, cfg S3Config) (*s3.Client, error) {
	var loadOpts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
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
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", localPath, err)
	}

	client, err := newClient(ctx, cfg)
	if err != nil {
		return 0, err
	}

	if _, err := manager.NewUploader(client).Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
		Body:   f,
	}); err != nil {
		return 0, fmt.Errorf("upload to s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	return fi.Size(), nil
}

// Download streams s3://<bucket>/<key> to outPath, using parallel ranged GETs
// for large objects.
func Download(ctx context.Context, cfg S3Config, key, outPath string) error {
	client, err := newClient(ctx, cfg)
	if err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer out.Close()

	// manager.Downloader writes concurrently and needs an io.WriterAt;
	// *os.File satisfies it.
	if _, err := manager.NewDownloader(client).Download(ctx, out, &s3.GetObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(key),
	}); err != nil {
		return fmt.Errorf("download s3://%s/%s: %w", cfg.Bucket, key, err)
	}
	return nil
}
