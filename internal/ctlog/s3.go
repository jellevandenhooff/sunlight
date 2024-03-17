package ctlog

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	awshttp "github.com/aws/smithy-go/transport/http"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type S3Backend struct {
	client        *s3.Client
	bucket        string
	keyPrefix     string
	metrics       []prometheus.Collector
	uploadSize    prometheus.Summary
	compressRatio prometheus.Summary
	hedgeRequests prometheus.Counter
	hedgeWins     prometheus.Counter
	log           *slog.Logger
}

func NewS3Backend(ctx context.Context, region, bucket, endpoint, keyPrefix string, l *slog.Logger) (*S3Backend, error) {
	counter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "s3_requests_total",
			Help: "S3 HTTP requests performed, by method and response code.",
		},
		[]string{"method", "code"},
	)
	duration := prometheus.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       "s3_request_duration_seconds",
			Help:       "S3 HTTP request latencies, by method and response code.",
			Objectives: map[float64]float64{0.5: 0.05, 0.75: 0.025, 0.9: 0.01, 0.99: 0.001},
			MaxAge:     1 * time.Minute,
			AgeBuckets: 6,
		},
		[]string{"method", "code"},
	)
	uploadSize := prometheus.NewSummary(
		prometheus.SummaryOpts{
			Name:       "s3_upload_size_bytes",
			Help:       "S3 (compressed) body size in bytes for object puts.",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
			MaxAge:     1 * time.Minute,
			AgeBuckets: 6,
		},
	)
	compressRatio := prometheus.NewSummary(
		prometheus.SummaryOpts{
			Name:       "s3_compress_ratio",
			Help:       "Ratio of compressed to uncompressed body size for compressible object puts.",
			MaxAge:     1 * time.Minute,
			AgeBuckets: 6,
		},
	)
	hedgeRequests := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "s3_hedges_total",
			Help: "S3 hedge requests that were launched because the main request was too slow.",
		},
	)
	hedgeWins := prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "s3_hedges_successful_total",
			Help: "S3 hedge requests that completed before the main request.",
		},
	)

	transport := http.RoundTripper(http.DefaultTransport.(*http.Transport).Clone())
	transport = promhttp.InstrumentRoundTripperCounter(counter, transport)
	transport = promhttp.InstrumentRoundTripperDuration(duration, transport)

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config for S3 backend: %w", err)
	}

	return &S3Backend{
		client: s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.Region = region
			if endpoint != "" {
				o.BaseEndpoint = aws.String(endpoint)
			}
			o.HTTPClient = &http.Client{Transport: transport}
			o.Retryer = retry.AddWithMaxBackoffDelay(retry.NewStandard(), 5*time.Millisecond)
		}),
		bucket:    bucket,
		keyPrefix: keyPrefix,
		metrics: []prometheus.Collector{counter, duration,
			uploadSize, compressRatio, hedgeRequests, hedgeWins},
		uploadSize:    uploadSize,
		compressRatio: compressRatio,
		hedgeRequests: hedgeRequests,
		hedgeWins:     hedgeWins,
		log:           l,
	}, nil
}

var _ Backend = &S3Backend{}

func (s *S3Backend) Upload(ctx context.Context, key string, data []byte, opts *UploadOptions) error {
	start := time.Now()
	contentType := aws.String("application/octet-stream")
	if opts != nil && opts.ContentType != "" {
		contentType = aws.String(opts.ContentType)
	}
	var contentEncoding *string
	if opts != nil && opts.Compress {
		b := &bytes.Buffer{}
		w := gzip.NewWriter(b)
		if _, err := w.Write(data); err != nil {
			return fmtErrorf("failed to compress %q: %w", key, err)
		}
		if err := w.Close(); err != nil {
			return fmtErrorf("failed to compress %q: %w", key, err)
		}
		s.compressRatio.Observe(float64(b.Len()) / float64(len(data)))
		data = b.Bytes()
		contentEncoding = aws.String("gzip")
	}
	var cacheControl *string
	if opts != nil && opts.Immutable {
		cacheControl = aws.String("public, max-age=604800, immutable")
	}
	putObject := func() (*s3.PutObjectOutput, error) {
		return s.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:          aws.String(s.bucket),
			Key:             aws.String(s.keyPrefix + key),
			Body:            bytes.NewReader(data),
			ContentLength:   aws.Int64(int64(len(data))),
			ContentEncoding: contentEncoding,
			ContentType:     contentType,
			CacheControl:    cacheControl,
		}, func(options *s3.Options) {
			// As an extra safety measure against concurrent sequencers (which are
			// especially likely on Fly), use Tigris conditional requests to only
			// create immutable objects if they don't exist yet. The LockBackend
			// protects against signing a split tree, but there is a risk that the
			// losing sequencer will overwrite the data tiles of the winning one.
			// Without S3 Versioning, that's potentially irrecoverable.
			if opts.Immutable && options.BaseEndpoint != nil &&
				*options.BaseEndpoint == "https://fly.storage.tigris.dev" {
				options.APIOptions = append(options.APIOptions, awshttp.AddHeaderValue("If-Match", ""))
			}
		})
	}
	ctx, cancel := context.WithCancelCause(ctx)
	hedgeErr := make(chan error, 1)
	go func() {
		timer := time.NewTimer(75 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			s.hedgeRequests.Inc()
			_, err := putObject()
			s.log.DebugContext(ctx, "S3 PUT hedge", "key", key, "err", err)
			hedgeErr <- err
			cancel(errors.New("competing request succeeded"))
		}
	}()
	_, err := putObject()
	select {
	case err = <-hedgeErr:
		s.hedgeWins.Inc()
	default:
		cancel(errors.New("competing request succeeded"))
	}
	s.log.DebugContext(ctx, "S3 PUT", "key", key, "size", len(data),
		"compress", contentEncoding != nil, "type", *contentType,
		"immutable", cacheControl != nil,
		"elapsed", time.Since(start), "err", err)
	s.uploadSize.Observe(float64(len(data)))
	if err != nil {
		return fmtErrorf("failed to upload %q to S3: %w", key, err)
	}
	return nil
}

func (s *S3Backend) Fetch(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.keyPrefix + key),
	})
	if err != nil {
		s.log.DebugContext(ctx, "S3 GET", "key", key, "err", err)
		return nil, fmtErrorf("failed to fetch %q from S3: %w", key, err)
	}
	defer out.Body.Close()
	s.log.DebugContext(ctx, "S3 GET", "key", key,
		"size", out.ContentLength, "encoding", out.ContentEncoding)
	body := out.Body
	if out.ContentEncoding != nil && *out.ContentEncoding == "gzip" {
		body, err = gzip.NewReader(out.Body)
		if err != nil {
			return nil, fmtErrorf("failed to decompress %q from S3: %w", key, err)
		}
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, fmtErrorf("failed to read %q from S3: %w", key, err)
	}
	return data, nil
}

func (s *S3Backend) List(ctx context.Context, prefix string) ([]string, error) {
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.keyPrefix + prefix),
	})
	if err != nil {
		s.log.DebugContext(ctx, "S3 LIST", "prefix", prefix, "err", err)
		return nil, fmtErrorf("failed to list %q from S3: %w", prefix, err)
	}
	s.log.DebugContext(ctx, "S3 LIST", "prefix", prefix,
		"count", len(out.Contents))

	var keys []string
	for _, object := range out.Contents {
		if object.Key == nil {
			return nil, fmtErrorf("failed to list %q from S3: nil key", prefix)
		}
		key := *object.Key
		if !strings.HasPrefix(key, s.keyPrefix+prefix) {
			return nil, fmtErrorf("failed to list %q from S3: strange response %q", prefix, key)
		}
		keys = append(keys, strings.TrimPrefix(key, s.keyPrefix))
	}
	if out.IsTruncated != nil && *out.IsTruncated {
		return nil, fmtErrorf("failed to list %q from S3: response truncated", prefix)
	}
	return keys, nil
}

func (s *S3Backend) Copy(ctx context.Context, from, to string) error {
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		CopySource: aws.String(s.bucket + "/" + s.keyPrefix + from),
		Key:        aws.String(s.keyPrefix + to),
	})
	if err != nil {
		s.log.DebugContext(ctx, "S3 COPY", "from", from, "to", to, "err", err)
		return fmtErrorf("failed to copy %q to %q on S3: %w", from, to, err)
	}
	s.log.DebugContext(ctx, "S3 COPY", "from", from, "to", to)
	return nil
}

func (s *S3Backend) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.keyPrefix + key),
	})
	if err != nil {
		s.log.DebugContext(ctx, "S3 DELETE", "key", key, "err", err)
		return fmtErrorf("failed to delete %q from S3: %w", key, err)
	}
	s.log.DebugContext(ctx, "S3 DELETE", "key", key)
	return nil
}

func (s *S3Backend) Metrics() []prometheus.Collector {
	return s.metrics
}
