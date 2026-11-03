package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// reviewKey names one review: the commit, the Radar that reviewed it and the
// reviewer configuration (configDigest).
type reviewKey struct {
	Repo, SHA, Version, Config string
}

func (k reviewKey) object(prefix, suffix string) string {
	return strings.Join([]string{prefix, strings.ToLower(k.Repo), k.SHA, k.Version, k.Config + suffix}, "/")
}

func (k reviewKey) String() string {
	return fmt.Sprintf("%s@%s (radar %s, config %s)", k.Repo, k.SHA, k.Version, k.Config)
}

// reviewRecord is a finished review: radar-gh review-commit's JSON output and
// who asked for it.
type reviewRecord struct {
	Repository   string          `json:"repository"`
	SHA          string          `json:"sha"`
	RadarVersion string          `json:"radar_version"`
	ConfigDigest string          `json:"config_digest"`
	RequestedBy  string          `json:"requested_by"`
	StartedAt    time.Time       `json:"started_at"`
	FinishedAt   time.Time       `json:"finished_at"`
	Result       json.RawMessage `json:"result"`
}

const (
	stateRunning = "running"
	stateFailed  = "failed"
	// stateRejected is a commit Radar cannot review. It is final for this
	// key: the commit, Radar and configuration are fixed, so a retry would
	// get the same answer and cost the person a review.
	stateRejected = "rejected"
)

// pendingRecord marks a review a replica has started, or one that failed.
// Writing it is how replicas agree that only one of them runs a review.
type pendingRecord struct {
	State       string    `json:"state"`
	RequestedBy string    `json:"requested_by"`
	Pod         string    `json:"pod"`
	StartedAt   time.Time `json:"started_at"`
	Error       string    `json:"error,omitempty"`
}

var errPreconditionFailed = errors.New("store: precondition failed")

type store interface {
	// result returns the finished review, or nil if there is none.
	result(ctx context.Context, k reviewKey) (*reviewRecord, error)
	// pending returns the marker and its version, or nil if there is none.
	pending(ctx context.Context, k reviewKey) (*pendingRecord, string, error)
	// claim writes p only if the marker is still at version (absent when
	// version is ""), returning the new version, or errPreconditionFailed if
	// another replica wrote it first. A write whose response was lost can
	// report an error, errPreconditionFailed included, though it committed;
	// server.claim reads the marker back.
	claim(ctx context.Context, k reviewKey, p pendingRecord, version string) (string, error)
	// fail records that the review this replica claimed at version failed,
	// unless another replica has since taken it over (errPreconditionFailed).
	fail(ctx context.Context, k reviewKey, p pendingRecord, version string) error
	// finish stores the result and removes the marker if it is still at
	// version. The result is what matters: a marker left behind is harmless,
	// as status reads the result first.
	finish(ctx context.Context, k reviewKey, r reviewRecord, version string) error
}

// s3Store keeps results as JSON objects. Conditional writes (If-None-Match,
// If-Match) make claiming a review atomic across replicas without a database.
type s3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

const (
	resultSuffix  = ".json"
	pendingSuffix = ".pending.json"
)

func (s *s3Store) result(ctx context.Context, k reviewKey) (*reviewRecord, error) {
	var r reviewRecord
	_, found, err := s.get(ctx, k.object(s.prefix, resultSuffix), &r)
	if err != nil || !found {
		return nil, err
	}
	return &r, nil
}

func (s *s3Store) pending(ctx context.Context, k reviewKey) (*pendingRecord, string, error) {
	var p pendingRecord
	etag, found, err := s.get(ctx, k.object(s.prefix, pendingSuffix), &p)
	if err != nil || !found {
		return nil, "", err
	}
	return &p, etag, nil
}

func (s *s3Store) claim(ctx context.Context, k reviewKey, p pendingRecord, version string) (string, error) {
	in := s.putInput(k.object(s.prefix, pendingSuffix), p)
	if version == "" {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = aws.String(version)
	}
	out, err := s.client.PutObject(ctx, in)
	if isPreconditionFailure(err) {
		return "", errPreconditionFailed
	}
	if err != nil {
		return "", err
	}
	return aws.ToString(out.ETag), nil
}

func (s *s3Store) fail(ctx context.Context, k reviewKey, p pendingRecord, version string) error {
	in := s.putInput(k.object(s.prefix, pendingSuffix), p)
	in.IfMatch = aws.String(version)
	_, err := s.client.PutObject(ctx, in)
	if isPreconditionFailure(err) {
		return errPreconditionFailed
	}
	return err
}

func (s *s3Store) finish(ctx context.Context, k reviewKey, r reviewRecord, version string) error {
	if _, err := s.client.PutObject(ctx, s.putInput(k.object(s.prefix, resultSuffix), r)); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(k.object(s.prefix, pendingSuffix)), IfMatch: aws.String(version),
	})
	if isPreconditionFailure(err) {
		return nil
	}
	return err
}

func (s *s3Store) putInput(key string, v any) *s3.PutObjectInput {
	b, _ := json.Marshal(v)
	return &s3.PutObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key),
		Body: bytes.NewReader(b), ContentType: aws.String("application/json"),
	}
}

func (s *s3Store) get(ctx context.Context, key string, v any) (etag string, found bool, err error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return "", false, nil
		}
		return "", false, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, maxResultBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > maxResultBytes {
		return "", false, fmt.Errorf("%s exceeds %d bytes", key, maxResultBytes)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return "", false, fmt.Errorf("%s: %w", key, err)
	}
	return aws.ToString(out.ETag), true, nil
}

// isPreconditionFailure reports S3's answers to a conditional write that lost:
// 412 when the object changed, 409 when another conditional write to the same
// key was in progress.
func isPreconditionFailure(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "PreconditionFailed", "ConditionalRequestConflict":
			return true
		}
	}
	var respErr interface{ HTTPStatusCode() int }
	if errors.As(err, &respErr) {
		code := respErr.HTTPStatusCode()
		return code == http.StatusPreconditionFailed || code == http.StatusConflict
	}
	return false
}

// memStore is the store in tests, with the same conditional-write semantics.
type memStore struct {
	mu       sync.Mutex
	results  map[reviewKey]reviewRecord
	markers  map[reviewKey]pendingRecord
	versions map[reviewKey]int
}

func newMemStore() *memStore {
	return &memStore{results: map[reviewKey]reviewRecord{}, markers: map[reviewKey]pendingRecord{}, versions: map[reviewKey]int{}}
}

func (m *memStore) result(_ context.Context, k reviewKey) (*reviewRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.results[k]
	if !ok {
		return nil, nil
	}
	return &r, nil
}

func (m *memStore) pending(_ context.Context, k reviewKey) (*pendingRecord, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.markers[k]
	if !ok {
		return nil, "", nil
	}
	return &p, fmt.Sprint(m.versions[k]), nil
}

func (m *memStore) matches(k reviewKey, version string) bool {
	_, exists := m.markers[k]
	if version == "" {
		return !exists
	}
	return exists && version == fmt.Sprint(m.versions[k])
}

func (m *memStore) claim(_ context.Context, k reviewKey, p pendingRecord, version string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.matches(k, version) {
		return "", errPreconditionFailed
	}
	m.markers[k] = p
	m.versions[k]++
	return fmt.Sprint(m.versions[k]), nil
}

func (m *memStore) fail(_ context.Context, k reviewKey, p pendingRecord, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.matches(k, version) {
		return errPreconditionFailed
	}
	m.markers[k] = p
	m.versions[k]++
	return nil
}

func (m *memStore) finish(_ context.Context, k reviewKey, r reviewRecord, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.results[k] = r
	if m.matches(k, version) {
		delete(m.markers, k)
		m.versions[k]++
	}
	return nil
}
