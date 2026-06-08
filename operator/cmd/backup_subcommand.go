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

package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/backup"
)

// s3Subcommands are the multi-call entrypoints the backup/restore Jobs invoke on
// the operator image (ADR-014). They run a one-shot S3 transfer and exit, rather
// than starting the controller manager.
var s3Subcommands = map[string]bool{
	"backup-upload":    true,
	"restore-download": true,
}

// maybeRunS3Subcommand dispatches to a one-shot S3 transfer when invoked as
// `manager <subcommand> ...`, then exits. It returns false (and does nothing)
// for the normal manager invocation so main() proceeds to start the manager.
func maybeRunS3Subcommand() bool {
	if len(os.Args) < 2 || !s3Subcommands[os.Args[1]] {
		return false
	}
	name := os.Args[1]

	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var (
		bucket   = fs.String("bucket", "", "S3 bucket (required)")
		endpoint = fs.String("endpoint", "", "S3 endpoint URL (empty for AWS S3)")
		region   = fs.String("region", "", "S3 region")
		key      = fs.String("key", "", "S3 object key (required)")
		file     = fs.String("file", "", "local file to upload (backup-upload)")
		out      = fs.String("out", "", "local path to write (restore-download)")
		insecure = fs.Bool("insecure-tls", false, "skip TLS verification against the endpoint (test-only)")
	)
	// flag.ExitOnError handles parse errors for us.
	_ = fs.Parse(os.Args[2:])

	if *bucket == "" || *key == "" {
		fmt.Fprintln(os.Stderr, "Error: --bucket and --key are required")
		os.Exit(2)
	}

	cfg := backup.S3Config{
		Bucket:      *bucket,
		Endpoint:    *endpoint,
		Region:      *region,
		InsecureTLS: *insecure,
	}
	ctx := context.Background()

	switch name {
	case "backup-upload":
		if *file == "" {
			fmt.Fprintln(os.Stderr, "Error: --file is required for backup-upload")
			os.Exit(2)
		}
		size, err := backup.Upload(ctx, cfg, *key, *file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("uploaded %d bytes to s3://%s/%s\n", size, *bucket, *key)
	case "restore-download":
		if *out == "" {
			fmt.Fprintln(os.Stderr, "Error: --out is required for restore-download")
			os.Exit(2)
		}
		if err := backup.Download(ctx, cfg, *key, *out); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("downloaded s3://%s/%s to %s\n", *bucket, *key, *out)
	}

	return true
}
