package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/vbauerster/mpb/v8"
)

var (
	retryAttempts    = 3
	retryBaseBackoff = time.Second
)

const defaultChunkSize = 50 * 1024 * 1024

type Credentials struct {
	Username string
	Password string
}

type countingReader struct {
	reader io.Reader
	bar    *mpb.Bar
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.reader.Read(p)
	if n > 0 && cr.bar != nil {
		cr.bar.IncrBy(n)
	}
	return n, err
}

func pushLayer(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	cred Credentials,
	digest v1.Hash,
	content io.Reader,
	totalSize int64,
	chunkSize int64,
	bar *mpb.Bar, //nolint:unparam // bar will be non-nil in orchestration task
) error {
	exists, err := blobExists(ctx, client, baseURL, cred, digest)
	if err != nil {
		return fmt.Errorf("checking blob existence: %w", err)
	}
	if exists {
		if bar != nil {
			bar.IncrBy(int(totalSize))
		}
		return nil
	}

	location, effectiveChunkSize, err := initiateUpload(ctx, client, baseURL, cred, chunkSize)
	if err != nil {
		return fmt.Errorf("initiating upload: %w", err)
	}

	if bar != nil {
		content = &countingReader{reader: content, bar: bar}
	}

	var offset int64
	for offset < totalSize {
		remaining := totalSize - offset
		size := effectiveChunkSize
		if remaining < size {
			size = remaining
		}

		chunk := make([]byte, size)
		n, err := io.ReadFull(content, chunk)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("reading chunk: %w", err)
		}
		chunk = chunk[:n]

		location, err = uploadChunk(ctx, client, location, cred, chunk, offset)
		if err != nil {
			return fmt.Errorf("uploading chunk at offset %d: %w", offset, err)
		}

		offset += int64(n)
	}

	if err := finalizeUpload(ctx, client, location, cred, digest); err != nil {
		return fmt.Errorf("finalizing upload: %w", err)
	}

	return nil
}

func blobExists(ctx context.Context, client *http.Client, baseURL string, cred Credentials, digest v1.Hash) (bool, error) {
	resp, err := doWithRetry(ctx, client, func() (*http.Request, error) {
		return newAuthenticatedRequest(ctx, http.MethodHead, baseURL+"/blobs/"+digest.String(), nil, cred)
	}, http.StatusNotFound)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

func initiateUpload(ctx context.Context, client *http.Client, baseURL string, cred Credentials, clientChunkSize int64) (uploadLocation string, effectiveChunkSize int64, _ error) {
	resp, err := doWithRetry(ctx, client, func() (*http.Request, error) {
		return newAuthenticatedRequest(ctx, http.MethodPost, baseURL+"/blobs/uploads/", nil, cred)
	})
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		return "", 0, fmt.Errorf("unexpected status %d from upload initiation", resp.StatusCode)
	}

	var err2 error
	uploadLocation, err2 = resolveLocation(baseURL+"/blobs/uploads/", resp.Header.Get("Location"))
	if err2 != nil {
		return "", 0, fmt.Errorf("resolving upload location: %w", err2)
	}

	effectiveChunkSize = clientChunkSize
	if maxStr := resp.Header.Get("OCI-Chunk-Max-Length"); maxStr != "" {
		if registryMax, parseErr := strconv.ParseInt(maxStr, 10, 64); parseErr == nil && registryMax > 0 {
			if effectiveChunkSize == 0 || registryMax < effectiveChunkSize {
				effectiveChunkSize = registryMax
			}
		}
	}
	if effectiveChunkSize <= 0 {
		effectiveChunkSize = defaultChunkSize
	}

	return uploadLocation, effectiveChunkSize, nil
}

func uploadChunk(ctx context.Context, client *http.Client, uploadURL string, cred Credentials, chunk []byte, offset int64) (string, error) {
	end := offset + int64(len(chunk)) - 1
	contentRange := fmt.Sprintf("%d-%d", offset, end)

	resp, err := doWithRetry(ctx, client, func() (*http.Request, error) {
		req, err := newAuthenticatedRequest(ctx, http.MethodPatch, uploadURL, bytes.NewReader(chunk), cred)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Content-Range", contentRange)
		req.ContentLength = int64(len(chunk))
		return req, nil
	})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		return "", fmt.Errorf("unexpected status %d from chunk upload", resp.StatusCode)
	}

	location, err := resolveLocation(uploadURL, resp.Header.Get("Location"))
	if err != nil {
		return "", fmt.Errorf("resolving chunk location: %w", err)
	}

	return location, nil
}

func finalizeUpload(ctx context.Context, client *http.Client, uploadURL string, cred Credentials, digest v1.Hash) error {
	finalURL, err := appendDigestParam(uploadURL, digest)
	if err != nil {
		return fmt.Errorf("building finalize URL: %w", err)
	}

	resp, err := doWithRetry(ctx, client, func() (*http.Request, error) {
		return newAuthenticatedRequest(ctx, http.MethodPut, finalURL, nil, cred)
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("unexpected status %d from upload finalization", resp.StatusCode)
	}

	return nil
}

func newAuthenticatedRequest(ctx context.Context, method, rawURL string, body io.Reader, cred Credentials) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cred.Username, cred.Password)
	return req, nil
}

func resolveLocation(base, location string) (string, error) {
	if location == "" {
		return "", fmt.Errorf("empty Location header")
	}

	locURL, err := url.Parse(location)
	if err != nil {
		return "", err
	}

	if locURL.IsAbs() {
		return location, nil
	}

	baseURL, err := url.Parse(base)
	if err != nil {
		return "", err
	}

	return baseURL.ResolveReference(locURL).String(), nil
}

func appendDigestParam(rawURL string, digest v1.Hash) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("digest", digest.String())
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func doWithRetry(ctx context.Context, client *http.Client, makeReq func() (*http.Request, error), acceptedCodes ...int) (*http.Response, error) {
	accepted := make(map[int]bool, len(acceptedCodes))
	for _, code := range acceptedCodes {
		accepted[code] = true
	}

	var lastErr error
	for attempt := range retryAttempts + 1 {
		if attempt > 0 {
			backoff := retryBaseBackoff * time.Duration(1<<uint(attempt-1))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := makeReq()
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		if accepted[resp.StatusCode] {
			return resp, nil
		}

		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("server error: HTTP %d", resp.StatusCode)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	return nil, fmt.Errorf("all %d attempts failed: %w", retryAttempts+1, lastErr)
}
