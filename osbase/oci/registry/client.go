// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

// Package registry contains a client and server implementation of the OCI
// Distribution spec. Both client and server only support pulling. The server is
// intended for use in tests.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff/v4"
	ocispecv1 "github.com/opencontainers/image-spec/specs-go/v1"

	"source.monogon.dev/osbase/oci"
)

// Sources for these expressions:
//
//   - https://github.com/opencontainers/distribution-spec/blob/main/spec.md#pulling-manifests
//   - https://github.com/opencontainers/image-spec/blob/main/descriptor.md#digests
const (
	repositoryExpr = `[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:\/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*`
	tagExpr        = `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`
	digestExpr     = `[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-zA-Z0-9=_-]+`
)

var (
	RepositoryRegexp = regexp.MustCompile(`^` + repositoryExpr + `$`)
	TagRegexp        = regexp.MustCompile(`^` + tagExpr + `$`)
	DigestRegexp     = regexp.MustCompile(`^` + digestExpr + `$`)
)

// unknownManifest can be used to parse the media type from a manifest of
// unknown type.
type unknownManifest struct {
	MediaType string `json:"mediaType,omitempty"`
}

// Client is an OCI registry client.
type Client struct {
	// Transport will be used to make requests. For example, this allows
	// configuring TLS client and CA certificates.
	// If nil, [http.DefaultTransport] is used.
	Transport http.RoundTripper
	// GetBackOff can be set to to make the Client retry HTTP requests.
	GetBackOff func() backoff.BackOff
	// RetryNotify receives errors that trigger a retry, e.g. for logging.
	RetryNotify backoff.Notify
	// UserAgent is used as the User-Agent HTTP header.
	UserAgent string

	// Scheme must be either http or https.
	Scheme string
	// Host is the host with optional port.
	Host string
	// Repository is the name of the repository. It is part of the client because
	// bearer tokens are usually scoped to a repository.
	Repository string

	authMu sync.RWMutex
	// bearerToken is a cached token obtained from an authorization service.
	bearerToken string
}

// Read fetches a manifest from the registry and returns an [oci.Ref].
//
// The context is used for the manifest request and for all blob and manifest
// requests made through the Ref.
//
// At least one of tag and digest must be set. If only tag is set, then you are
// trusting the registry to return the right content. Otherwise, the digest is
// used to verify the manifest. If both tag and digest are set, then the tag is
// used in the request, and the digest is used to verify the response. The
// advantage of fetching by tag is that it allows a pull through cache to
// display tags to a user inspecting the cache contents.
func (c *Client) Read(ctx context.Context, tag, digest string) (oci.Ref, error) {
	if !RepositoryRegexp.MatchString(c.Repository) {
		return nil, fmt.Errorf("invalid repository %q", c.Repository)
	}
	if tag != "" && !TagRegexp.MatchString(tag) {
		return nil, fmt.Errorf("invalid tag %q", tag)
	}
	if digest != "" {
		if _, _, err := oci.ParseDigest(digest); err != nil {
			return nil, err
		}
	}
	var reference string
	if tag != "" {
		reference = tag
	} else if digest != "" {
		reference = digest
	} else {
		return nil, fmt.Errorf("tag and digest cannot both be empty")
	}

	manifestPath := fmt.Sprintf("/v2/%s/manifests/%s", c.Repository, reference)
	var manifestBytes []byte
	var manifestMediaType string
	err := c.retry(ctx, func() error {
		req, err := c.newGet(manifestPath)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", ocispecv1.MediaTypeImageManifest+","+ocispecv1.MediaTypeImageIndex)
		resp, err := c.doGet(ctx, req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return readClientError(resp, req)
		}
		defer resp.Body.Close()
		manifestMediaType = resp.Header.Get("Content-Type")
		manifestBytes, err = readFullBody(resp, 50*1024*1024)
		return err
	})
	if err != nil {
		return nil, err
	}

	// Remove any parameters from the Content-Type header.
	manifestMediaType, _, _ = strings.Cut(manifestMediaType, ";")
	switch manifestMediaType {
	case ocispecv1.MediaTypeImageManifest, ocispecv1.MediaTypeImageIndex:
		// The Content-Type header is valid, use it.
	default:
		// We need to parse the manifest to extract the media type, then parse it
		// again for that media type.
		var manifest unknownManifest
		if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
			return nil, fmt.Errorf("failed to parse manifest: %w", err)
		}
		manifestMediaType = manifest.MediaType
	}

	blobs := &clientBlobs{
		ctx:    ctx,
		client: c,
	}
	return oci.NewRef(manifestBytes, manifestMediaType, digest, blobs)
}

type clientBlobs struct {
	ctx    context.Context
	client *Client
}

func (r *clientBlobs) Manifest(descriptor *ocispecv1.Descriptor) ([]byte, error) {
	digest := string(descriptor.Digest)
	if _, _, err := oci.ParseDigest(digest); err != nil {
		return nil, err
	}

	manifestPath := fmt.Sprintf("/v2/%s/manifests/%s", r.client.Repository, digest)
	var manifestBytes []byte
	err := r.client.retry(r.ctx, func() error {
		req, err := r.client.newGet(manifestPath)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", ocispecv1.MediaTypeImageManifest+","+ocispecv1.MediaTypeImageIndex)
		resp, err := r.client.doGet(r.ctx, req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return readClientError(resp, req)
		}
		defer resp.Body.Close()
		manifestBytes, err = readKnownSizeBody(resp, int(descriptor.Size))
		return err
	})
	if err != nil {
		return nil, err
	}
	return manifestBytes, nil
}

func (r *clientBlobs) Blobs(_ *ocispecv1.Descriptor) (oci.Blobs, error) {
	return r, nil
}

func (r *clientBlobs) Blob(descriptor *ocispecv1.Descriptor) (io.ReadCloser, error) {
	if !DigestRegexp.MatchString(string(descriptor.Digest)) {
		return nil, fmt.Errorf("invalid blob digest %q", descriptor.Digest)
	}
	blobPath := fmt.Sprintf("/v2/%s/blobs/%s", r.client.Repository, descriptor.Digest)
	var resp *http.Response
	err := r.client.retry(r.ctx, func() error {
		req, err := r.client.newGet(blobPath)
		if err != nil {
			return err
		}
		resp, err = r.client.doGet(r.ctx, req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			return readClientError(resp, req)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if r.client.GetBackOff == nil {
		return resp.Body, nil
	}
	ctx, cancel := context.WithCancelCause(r.ctx)
	reader := &retryReader{
		ctx:    ctx,
		cancel: cancel,
		client: r.client,
		path:   blobPath,
		pos:    0,
		size:   descriptor.Size,
	}
	reader.resp.Store(resp)
	return reader, nil
}

type retryReader struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	client *Client
	path   string
	pos    int64
	size   int64
	// resp is an atomic pointer because it may be concurrently written by Read()
	// and read by Close().
	resp atomic.Pointer[http.Response]
}

func (r *retryReader) Read(p []byte) (n int, err error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > r.size-r.pos {
		p = p[:r.size-r.pos]
	}
	closed := false
	err = r.client.retry(r.ctx, func() error {
		if closed {
			req, err := r.client.newGet(r.path)
			if err != nil {
				return err
			}
			if r.pos != 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", r.pos))
			}
			resp, err := r.client.doGet(r.ctx, req)
			if err != nil {
				return err
			}
			r.resp.Store(resp)
			if err := context.Cause(r.ctx); err != nil {
				resp.Body.Close()
				return err
			}
			switch resp.StatusCode {
			case http.StatusOK:
				_, err := io.CopyN(io.Discard, resp.Body, r.pos)
				if err != nil {
					return err
				}
			case http.StatusPartialContent:
				if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", r.pos)) {
					return backoff.Permanent(errors.New("invalid content range"))
				}
			default:
				return readClientError(resp, req)
			}
		}
		var err error
		n, err = r.resp.Load().Body.Read(p)
		if n != 0 {
			r.pos += int64(n)
			return nil
		}
		if err == nil {
			err = errors.New("read 0 bytes")
		}
		closed = true
		r.resp.Load().Body.Close()
		return err
	})
	if r.pos >= r.size {
		err = io.EOF
	} else if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return
}

func (r *retryReader) Close() error {
	r.cancel(errors.New("reader closed"))
	return r.resp.Load().Body.Close()
}

func (c *Client) retry(ctx context.Context, o func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var b backoff.BackOff
	for {
		err := o()
		if err == nil {
			return nil
		}
		var permanent *backoff.PermanentError
		if errors.As(err, &permanent) {
			return err
		}
		if ctx.Err() != nil {
			return err
		}
		if b == nil {
			if c.GetBackOff == nil {
				return err
			}
			b = c.GetBackOff()
		}
		next := b.NextBackOff()
		if next == backoff.Stop {
			return err
		}
		var clientErr *ClientError
		if errors.As(err, &clientErr) && !clientErr.RetryAfter.IsZero() {
			next = max(next, time.Until(clientErr.RetryAfter))
		}
		deadline, hasDeadline := ctx.Deadline()
		if hasDeadline && time.Until(deadline) < next {
			return err
		}

		if c.RetryNotify != nil {
			c.RetryNotify(err, next)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(next):
		}
	}
}

func (c *Client) newGet(path string) (*http.Request, error) {
	u := url.URL{
		Scheme: c.Scheme,
		Host:   c.Host,
		Path:   path,
	}
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	return req, nil
}

func (c *Client) doGet(ctx context.Context, req *http.Request) (*http.Response, error) {
	req = req.WithContext(ctx)
	c.addAuthorization(req)
	client := http.Client{Transport: c.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, redactURLError(err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		unauthorizedErr := readClientError(resp, req)
		retry, err := c.handleUnauthorized(ctx, resp)
		if err != nil {
			return nil, err
		}
		if !retry {
			return nil, unauthorizedErr
		}
		c.addAuthorization(req)
		resp, err = client.Do(req)
		if err != nil {
			return nil, redactURLError(err)
		}
	}

	return resp, nil
}

func readClientError(resp *http.Response, req *http.Request) error {
	defer resp.Body.Close()
	clientErr := &ClientError{
		StatusCode: resp.StatusCode,
	}
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter != "" {
		seconds, err := strconv.ParseInt(retryAfter, 10, 64)
		if err == nil {
			clientErr.RetryAfter = time.Now().Add(time.Duration(seconds) * time.Second)
		} else {
			clientErr.RetryAfter, _ = http.ParseTime(retryAfter)
		}
	}
	content, err := readFullBody(resp, 2048)
	if err == nil {
		clientErr.RawBody = content
		_ = json.Unmarshal(content, &clientErr.ErrorBody)
	}

	errReq := resp.Request
	if errReq == nil {
		errReq = req
	}
	urlErr := &url.Error{
		Op:  errReq.Method,
		URL: errReq.URL.Redacted(),
		Err: clientErr,
	}
	err = redactURLError(urlErr)

	// Client errors are usually permanent, and server errors are usually
	// temporary, but there are some exceptions.
	isTemporary := 500 <= clientErr.StatusCode && clientErr.StatusCode <= 599
	switch clientErr.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooEarly,
		http.StatusTooManyRequests,
		499: // nginx-specific, client closed request
		isTemporary = true
	case http.StatusNotImplemented, http.StatusHTTPVersionNotSupported,
		http.StatusNetworkAuthenticationRequired:
		isTemporary = false
	}
	if !isTemporary {
		return backoff.Permanent(err)
	}
	return err
}

// ClientError is an HTTP error received from a registry or authorization
// service.
type ClientError struct {
	ErrorBody
	StatusCode int
	RetryAfter time.Time
	RawBody    []byte
}

type ErrorBody struct {
	Errors []ErrorInfo `json:"errors,omitempty"`
}

type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func (e *ClientError) Error() string {
	if len(e.Errors) == 0 {
		text := fmt.Sprintf("HTTP %d %s", e.StatusCode, http.StatusText(e.StatusCode))
		if len(e.RawBody) != 0 {
			text = fmt.Sprintf("%s: %q", text, e.RawBody)
		}
		return text
	}
	var errorStrs []string
	for _, ei := range e.Errors {
		errorStrs = append(errorStrs, fmt.Sprintf("%s: %s", ei.Code, ei.Message))
	}
	return fmt.Sprintf("HTTP %d %s", e.StatusCode, strings.Join(errorStrs, "; "))
}

// redactURLError redacts the URL in an [url.Error]. After redirects, the URL
// may contain secrets in query parameter values.
//
// Logic adapted from:
// https://github.com/google/go-containerregistry/blob/v0.20.3/internal/redact/redact.go
func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	u, perr := url.Parse(urlErr.URL)
	if perr != nil {
		return err
	}
	query := u.Query()
	for name, vals := range query {
		if name == "scope" || name == "service" {
			continue
		}
		for i := range vals {
			vals[i] = "REDACTED"
		}
	}
	u.RawQuery = query.Encode()
	urlErr.URL = u.Redacted()
	return err
}

func readFullBody(resp *http.Response, limit int) ([]byte, error) {
	switch {
	case resp.ContentLength < 0:
		lr := io.LimitReader(resp.Body, int64(limit)+1)
		content, err := io.ReadAll(lr)
		if err != nil {
			return nil, err
		}
		if len(content) > limit {
			return nil, backoff.Permanent(fmt.Errorf("HTTP response exceeds limit of %d bytes", limit))
		}
		return content, nil
	case resp.ContentLength <= int64(limit):
		content := make([]byte, resp.ContentLength)
		_, err := io.ReadFull(resp.Body, content)
		if err != nil {
			return nil, err
		}
		return content, nil
	default:
		return nil, backoff.Permanent(fmt.Errorf("HTTP response of size %d exceeds limit of %d bytes", resp.ContentLength, limit))
	}
}

func readKnownSizeBody(resp *http.Response, size int) ([]byte, error) {
	if resp.ContentLength >= 0 && resp.ContentLength != int64(size) {
		return nil, backoff.Permanent(fmt.Errorf("HTTP response has size %d, expected %d bytes", resp.ContentLength, size))
	}
	content := make([]byte, size)
	_, err := io.ReadFull(resp.Body, content)
	if err != nil {
		return nil, err
	}
	return content, nil
}
