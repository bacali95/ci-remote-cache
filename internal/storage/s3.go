package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3 stores cache artifacts in an S3-compatible bucket (AWS S3, Cloudflare
// R2, MinIO, ...). This is the recommended backend for CI runners: it
// survives ephemeral hosts and scales to multiple server replicas behind a
// load balancer.
type S3 struct {
	client *s3.Client
	bucket string
	prefix string
}

type S3Options struct {
	Bucket       string
	Region       string
	Prefix       string
	Endpoint     string // optional: non-AWS S3-compatible endpoint
	UsePathStyle bool

	// AccessKeyID/SecretAccessKey are optional static credentials (e.g.
	// entered via the admin Settings UI). If either is empty, the AWS
	// default credential chain is used instead (env vars, IAM role,
	// shared config file, ...).
	AccessKeyID     string
	SecretAccessKey string
}

func NewS3(ctx context.Context, opts S3Options) (*S3, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}
	if opts.AccessKeyID != "" && opts.SecretAccessKey != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKeyID, opts.SecretAccessKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.UsePathStyle
	})

	return &S3{client: client, bucket: opts.Bucket, prefix: opts.Prefix}, nil
}

func (s *S3) key(hash string) string {
	if s.prefix == "" {
		return hash
	}
	return path.Join(s.prefix, hash)
}

func (s *S3) unkey(key string) string {
	if s.prefix == "" {
		return key
	}
	return strings.TrimPrefix(key, s.prefix+"/")
}

func (s *S3) Exists(ctx context.Context, hash string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(hash)),
	})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *S3) Put(ctx context.Context, hash string, r io.Reader, size int64) error {
	exists, err := s.Exists(ctx, hash)
	if err != nil {
		return err
	}
	if exists {
		return ErrAlreadyExists
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(s.key(hash)),
		Body:          io.LimitReader(r, size),
		ContentLength: aws.Int64(size),
	})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(hash)),
	})
	if isNotFound(err) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("get object: %w", err)
	}
	size := int64(0)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}
	return out.Body, size, nil
}

func (s *S3) Delete(ctx context.Context, hash string) error {
	exists, err := s.Exists(ctx, hash)
	if err != nil {
		return err
	}
	if !exists {
		return ErrNotFound
	}
	_, err = s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(hash)),
	})
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func (s *S3) List(ctx context.Context, cursor string, limit int) (ListPage, error) {
	if limit <= 0 {
		limit = defaultListLimit
	}

	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		MaxKeys: aws.Int32(int32(limit)),
	}
	if s.prefix != "" {
		input.Prefix = aws.String(s.prefix + "/")
	}
	if cursor != "" {
		input.ContinuationToken = aws.String(cursor)
	}

	out, err := s.client.ListObjectsV2(ctx, input)
	if err != nil {
		return ListPage{}, fmt.Errorf("list objects: %w", err)
	}

	page := ListPage{}
	for _, obj := range out.Contents {
		if obj.Key == nil {
			continue
		}
		entry := Entry{Key: s.unkey(*obj.Key)}
		if obj.Size != nil {
			entry.Size = *obj.Size
		}
		if obj.LastModified != nil {
			entry.ModTime = *obj.LastModified
		}
		page.Entries = append(page.Entries, entry)
	}
	if out.NextContinuationToken != nil {
		page.NextCursor = *out.NextContinuationToken
	}
	return page, nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	return false
}
