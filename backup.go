package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// R2Client wraps an S3-compatible client pointed at Cloudflare R2.
type R2Client struct {
	s3       *s3.Client
	uploader *manager.Uploader
	bucket   string
}

// NewR2Client creates an R2Client from Config. Returns an error (not a panic)
// when credentials are missing so the binary can start without backups configured.
func NewR2Client(cfg Config) (*R2Client, error) {
	if cfg.R2AccountID == "" || cfg.R2AccessKeyID == "" || cfg.R2SecretAccessKey == "" {
		return nil, fmt.Errorf("R2 credentials not configured (R2_ACCOUNT_ID / R2_ACCESS_KEY_ID / R2_SECRET_ACCESS_KEY)")
	}

	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.R2AccountID)

	awsCfg := aws.Config{
		Region: "auto",
		Credentials: credentials.NewStaticCredentialsProvider(
			cfg.R2AccessKeyID,
			cfg.R2SecretAccessKey,
			"",
		),
	}

	s3Client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	return &R2Client{
		s3:       s3Client,
		uploader: manager.NewUploader(s3Client, func(u *manager.Uploader) { u.PartSize = 10 * 1024 * 1024 }),
		bucket:   cfg.R2Bucket,
	}, nil
}

// Upload streams reader to R2 at the given key using multipart upload.
// No size limit and no in-memory buffering — safe for large backup streams.
func (r *R2Client) Upload(ctx context.Context, key string, reader io.Reader, contentType string) error {
	_, err := r.uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(r.bucket),
		Key:         aws.String(key),
		Body:        reader,
		ContentType: aws.String(contentType),
	})
	return err
}

// BackupEntry holds metadata for a single object returned by List.
type BackupEntry struct {
	Key          string
	LastModified time.Time
	Size         int64
}

// List returns ALL objects whose key starts with prefix, handling pagination.
// S3/R2 returns at most 1000 keys per call; this method follows continuation tokens.
func (r *R2Client) List(ctx context.Context, prefix string) ([]BackupEntry, error) {
	var entries []BackupEntry
	var continuationToken *string

	for {
		resp, err := r.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(r.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, err
		}

		for _, obj := range resp.Contents {
			entries = append(entries, BackupEntry{
				Key:          aws.ToString(obj.Key),
				LastModified: aws.ToTime(obj.LastModified),
				Size:         aws.ToInt64(obj.Size),
			})
		}

		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		continuationToken = resp.NextContinuationToken
	}
	return entries, nil
}

// DeleteOlderThan deletes all objects under prefix that are older than `days` days.
// Logs and continues on individual delete failures so one bad object does not
// block cleanup of all the rest.
func (r *R2Client) DeleteOlderThan(ctx context.Context, prefix string, days int) error {
	entries, err := r.List(ctx, prefix)
	if err != nil {
		return err
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	var lastErr error
	for _, e := range entries {
		if e.LastModified.Before(cutoff) {
			if delErr := r.deleteObject(ctx, e.Key); delErr != nil {
				// log and continue — one failure must not block the rest
				log.Printf("[r2] cleanup: failed to delete %s: %v", e.Key, delErr)
				lastErr = delErr
			} else {
				log.Printf("[r2] cleanup: deleted %s", e.Key)
			}
		}
	}
	return lastErr
}

// deleteObject removes a single object from R2.
func (r *R2Client) deleteObject(ctx context.Context, key string) error {
	_, err := r.s3.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(r.bucket),
		Key:    aws.String(key),
	})
	return err
}

// ── Key / path helpers ──────────────────────────────────────────────────────

// keyForDB returns the R2 object key for a database backup.
// e.g. databases/mysite/2026-03-03.sql.gz
func keyForDB(site, date string) string {
	return fmt.Sprintf("databases/%s/%s.sql.gz", site, date)
}

// keyForVolume returns the R2 object key for a volume backup.
// e.g. volumes/mysite/2026-03-03.tar.gz
func keyForVolume(site, date string) string {
	return fmt.Sprintf("volumes/%s/%s.tar.gz", site, date)
}

// prefixForSiteDB returns the R2 prefix to list all DB backups for a site.
func prefixForSiteDB(site string) string {
	return fmt.Sprintf("databases/%s/", site)
}

// prefixForSiteVolume returns the R2 prefix to list all volume backups for a site.
func prefixForSiteVolume(site string) string {
	return fmt.Sprintf("volumes/%s/", site)
}

// dateStamp returns today's UTC date as YYYY-MM-DD.
func dateStamp() string {
	return time.Now().UTC().Format("2006-01-02")
}

// dateFromKey extracts the YYYY-MM-DD date from an R2 object key.
// e.g. "databases/mysite/2026-03-03.sql.gz" → "2026-03-03"
func dateFromKey(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) == 0 {
		return ""
	}
	name := parts[len(parts)-1]
	name = strings.TrimSuffix(name, ".sql.gz")
	name = strings.TrimSuffix(name, ".tar.gz")
	return name
}

// parseDSNCredentials extracts user and password from a DSN of the form:
// user:pass@tcp(host:port)/
// Safe: looks for @tcp( to isolate the credentials prefix.
func parseDSNCredentials(dsn string) (user, pass string) {
	atTCP := strings.Index(dsn, "@tcp(")
	if atTCP < 0 {
		return "", ""
	}
	creds := dsn[:atTCP]
	colon := strings.Index(creds, ":")
	if colon < 0 {
		return creds, ""
	}
	return creds[:colon], creds[colon+1:]
}

// stripDockerMux strips the 8-byte Docker stream multiplexing header from each
// frame, returning a reader of raw stdout bytes.
//
// Docker's ContainerAttach response format (per frame):
//
//	[stream_type(1)] [pad(3)] [payload_size(4)] [payload...]
//
// stream_type: 1 = stdout, 2 = stderr. We forward stdout only.
func stripDockerMux(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		header := make([]byte, 8)
		for {
			_, err := io.ReadFull(r, header)
			if err != nil {
				// io.EOF means the Docker stream closed cleanly (container exited).
				// Call Close() (not CloseWithError) so the reader sees a normal EOF.
				if err == io.EOF {
					pw.Close()
				} else {
					pw.CloseWithError(err)
				}
				return
			}
			size := int64(header[4])<<24 | int64(header[5])<<16 | int64(header[6])<<8 | int64(header[7])
			if header[0] == 1 { // stdout
				if _, err := io.CopyN(pw, r, size); err != nil {
					pw.CloseWithError(err)
					return
				}
			} else {
				if _, err := io.CopyN(io.Discard, r, size); err != nil {
					pw.CloseWithError(err)
					return
				}
			}
		}
	}()
	return pr
}
