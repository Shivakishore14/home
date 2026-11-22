// Package storage provides presigned object storage (Cloudflare R2 / S3) for
// task inputs and outputs. File bytes never pass through the TaskEngine
// server: clients and workers upload/download directly with presigned URLs.
package storage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// ErrNotFound is returned by Head when the object does not exist.
var ErrNotFound = errors.New("object not found")

// PresignedRequest describes an HTTP request a client must send as-is.
type PresignedRequest struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// ObjectInfo is the subset of object metadata we care about.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Store is implemented by object storage backends.
type Store interface {
	// PresignPut returns a PUT request uploading exactly size bytes of contentType to key.
	PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (*PresignedRequest, error)
	// PresignGet returns a GET URL for key. filename (optional) sets Content-Disposition.
	PresignGet(ctx context.Context, key, filename string, attachment bool, ttl time.Duration) (string, error)
	// Head returns object metadata or ErrNotFound.
	Head(ctx context.Context, key string) (*ObjectInfo, error)
	// Delete removes an object (missing objects are not an error).
	Delete(ctx context.Context, key string) error
}

// R2Config configures an R2 (S3-compatible) store.
type R2Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	// Endpoint overrides https://<account>.r2.cloudflarestorage.com (tests, other S3 providers).
	Endpoint string
}

// R2ConfigFromEnv reads R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, R2_SECRET_ACCESS_KEY,
// R2_BUCKET and optional R2_ENDPOINT. ok is false when credentials are absent.
func R2ConfigFromEnv() (cfg R2Config, ok bool) {
	cfg = R2Config{
		AccountID:       strings.TrimSpace(os.Getenv("R2_ACCOUNT_ID")),
		AccessKeyID:     strings.TrimSpace(os.Getenv("R2_ACCESS_KEY_ID")),
		SecretAccessKey: strings.TrimSpace(os.Getenv("R2_SECRET_ACCESS_KEY")),
		Bucket:          strings.TrimSpace(os.Getenv("R2_BUCKET")),
		Endpoint:        strings.TrimSpace(os.Getenv("R2_ENDPOINT")),
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "taskengine"
	}
	ok = cfg.AccessKeyID != "" && cfg.SecretAccessKey != "" && (cfg.AccountID != "" || cfg.Endpoint != "")
	return cfg, ok
}

type r2Store struct {
	bucket  string
	client  *s3.Client
	presign *s3.PresignClient
}

// NewR2 creates a Store backed by Cloudflare R2.
func NewR2(cfg R2Config) (Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("r2: bucket is required")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		if cfg.AccountID == "" {
			return nil, fmt.Errorf("r2: account id or endpoint is required")
		}
		endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	}

	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		UsePathStyle: true,
		// R2 does not need (and older versions rejected) the SDK's default
		// CRC checksums; they would also leak into presigned URLs.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &r2Store{bucket: cfg.Bucket, client: client, presign: s3.NewPresignClient(client)}, nil
}

func (s *r2Store) PresignPut(ctx context.Context, key, contentType string, size int64, ttl time.Duration) (*PresignedRequest, error) {
	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	}
	if size > 0 {
		in.ContentLength = aws.Int64(size)
	}
	req, err := s.presign.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return nil, fmt.Errorf("presign put: %w", err)
	}
	headers := map[string]string{}
	for k, v := range req.SignedHeader {
		// Host and Content-Length are set by the HTTP client itself (browsers
		// refuse to set them); they are still enforced by the signature.
		if strings.EqualFold(k, "Host") || strings.EqualFold(k, "Content-Length") || len(v) == 0 {
			continue
		}
		headers[http.CanonicalHeaderKey(k)] = v[0]
	}
	return &PresignedRequest{Method: http.MethodPut, URL: req.URL, Headers: headers, ExpiresAt: time.Now().Add(ttl).UTC()}, nil
}

func (s *r2Store) PresignGet(ctx context.Context, key, filename string, attachment bool, ttl time.Duration) (string, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if filename != "" {
		disp := "inline"
		if attachment {
			disp = "attachment"
		}
		in.ResponseContentDisposition = aws.String(fmt.Sprintf(`%s; filename="%s"`, disp, strings.ReplaceAll(filename, `"`, "")))
	}
	req, err := s.presign.PresignGetObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("presign get: %w", err)
	}
	return req.URL, nil
}

func (s *r2Store) Head(ctx context.Context, key string) (*ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		var re *smithyhttp.ResponseError
		if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("head %s: %w", key, err)
	}
	info := &ObjectInfo{Size: aws.ToInt64(out.ContentLength), ContentType: aws.ToString(out.ContentType)}
	return info, nil
}

func (s *r2Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}
