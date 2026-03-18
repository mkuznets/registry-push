package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestBlob(content []byte) ([]byte, v1.Hash) {
	h := sha256.Sum256(content)
	return content, v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(h[:])}
}

func TestPushLayer(t *testing.T) {
	origBackoff := retryBaseBackoff
	retryBaseBackoff = time.Millisecond
	t.Cleanup(func() { retryBaseBackoff = origBackoff })

	cred := Credentials{Username: "testuser", Password: "testpass"}

	t.Run("full upload flow", func(t *testing.T) {
		data, digest := makeTestBlob(bytes.Repeat([]byte("x"), 100))

		var mu sync.Mutex
		var receivedData bytes.Buffer
		var methods []string

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			methods = append(methods, r.Method)
			mu.Unlock()

			user, pass, ok := r.BasicAuth()
			assert.True(t, ok, "basic auth should be present")
			assert.Equal(t, "testuser", user)
			assert.Equal(t, "testpass", pass)

			switch r.Method {
			case http.MethodHead:
				w.WriteHeader(http.StatusNotFound)
			case http.MethodPost:
				w.Header().Set("Location", "/v2/test/blobs/uploads/test-uuid")
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPatch:
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				receivedData.Write(body)
				mu.Unlock()
				assert.Equal(t, "application/octet-stream", r.Header.Get("Content-Type"))
				w.Header().Set("Location", r.URL.RequestURI())
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPut:
				assert.Contains(t, r.URL.Query().Get("digest"), digest.String())
				w.WriteHeader(http.StatusCreated)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer server.Close()

		err := pushLayer(t.Context(), server.Client(), server.URL+"/v2/test", cred, digest, bytes.NewReader(data), int64(len(data)), 0, nil)
		require.NoError(t, err)

		mu.Lock()
		assert.Equal(t, data, receivedData.Bytes())
		assert.Equal(t, []string{http.MethodHead, http.MethodPost, http.MethodPatch, http.MethodPut}, methods)
		mu.Unlock()
	})

	t.Run("skip existing blob", func(t *testing.T) {
		data, digest := makeTestBlob([]byte("already exists"))
		var requestCount atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCount.Add(1)
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusOK)
				return
			}
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}))
		defer server.Close()

		err := pushLayer(t.Context(), server.Client(), server.URL+"/v2/test", cred, digest, bytes.NewReader(data), int64(len(data)), 0, nil)
		require.NoError(t, err)
		assert.Equal(t, int32(1), requestCount.Load())
	})

	t.Run("retry on 5xx", func(t *testing.T) {
		data, digest := makeTestBlob([]byte("retry me"))
		var patchCount atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodHead:
				w.WriteHeader(http.StatusNotFound)
			case http.MethodPost:
				w.Header().Set("Location", "/v2/test/blobs/uploads/uuid")
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPatch:
				count := patchCount.Add(1)
				if count == 1 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", r.URL.RequestURI())
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPut:
				w.WriteHeader(http.StatusCreated)
			}
		}))
		defer server.Close()

		err := pushLayer(t.Context(), server.Client(), server.URL+"/v2/test", cred, digest, bytes.NewReader(data), int64(len(data)), 0, nil)
		require.NoError(t, err)
		assert.Equal(t, int32(2), patchCount.Load())
	})

	t.Run("fail on 4xx", func(t *testing.T) {
		data, digest := makeTestBlob([]byte("unauthorized"))

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodHead:
				w.WriteHeader(http.StatusNotFound)
			case http.MethodPost:
				w.WriteHeader(http.StatusUnauthorized)
			default:
				t.Errorf("unexpected request after 401: %s %s", r.Method, r.URL.Path)
			}
		}))
		defer server.Close()

		err := pushLayer(t.Context(), server.Client(), server.URL+"/v2/test", cred, digest, bytes.NewReader(data), int64(len(data)), 0, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "401")
	})

	t.Run("multiple chunks", func(t *testing.T) {
		data, digest := makeTestBlob(bytes.Repeat([]byte("abcdefghij"), 10))

		var mu sync.Mutex
		var receivedData bytes.Buffer
		var contentRanges []string
		var patchCount atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodHead:
				w.WriteHeader(http.StatusNotFound)
			case http.MethodPost:
				w.Header().Set("Location", "/v2/test/blobs/uploads/uuid")
				w.Header().Set("OCI-Chunk-Max-Length", "30")
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPatch:
				patchCount.Add(1)
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				receivedData.Write(body)
				contentRanges = append(contentRanges, r.Header.Get("Content-Range"))
				mu.Unlock()
				w.Header().Set("Location", r.URL.RequestURI())
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPut:
				w.WriteHeader(http.StatusCreated)
			}
		}))
		defer server.Close()

		err := pushLayer(t.Context(), server.Client(), server.URL+"/v2/test", cred, digest, bytes.NewReader(data), int64(len(data)), 0, nil)
		require.NoError(t, err)

		assert.Equal(t, int32(4), patchCount.Load())
		assert.Equal(t, data, receivedData.Bytes())

		mu.Lock()
		assert.Equal(t, []string{"0-29", "30-59", "60-89", "90-99"}, contentRanges)
		mu.Unlock()
	})

	t.Run("client chunk size caps registry max", func(t *testing.T) {
		data, digest := makeTestBlob(bytes.Repeat([]byte("z"), 100))
		var patchCount atomic.Int32

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodHead:
				w.WriteHeader(http.StatusNotFound)
			case http.MethodPost:
				w.Header().Set("Location", "/v2/test/blobs/uploads/uuid")
				w.Header().Set("OCI-Chunk-Max-Length", "1000")
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPatch:
				patchCount.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", r.URL.RequestURI())
				w.WriteHeader(http.StatusAccepted)
			case http.MethodPut:
				w.WriteHeader(http.StatusCreated)
			}
		}))
		defer server.Close()

		err := pushLayer(t.Context(), server.Client(), server.URL+"/v2/test", cred, digest, bytes.NewReader(data), int64(len(data)), 25, nil)
		require.NoError(t, err)
		assert.Equal(t, int32(4), patchCount.Load())
	})
}

func TestDoWithRetry(t *testing.T) {
	origBackoff := retryBaseBackoff
	retryBaseBackoff = time.Millisecond
	t.Cleanup(func() { retryBaseBackoff = origBackoff })

	t.Run("success on 2xx", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		resp, err := doWithRetry(t.Context(), server.Client(), func() (*http.Request, error) {
			return http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
		})
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("accepted non-2xx code", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		resp, err := doWithRetry(t.Context(), server.Client(), func() (*http.Request, error) {
			return http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
		}, http.StatusNotFound)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("fail on 4xx without retry", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusForbidden)
		}))
		defer server.Close()

		_, err := doWithRetry(t.Context(), server.Client(), func() (*http.Request, error) {
			return http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "403")
		assert.Equal(t, int32(1), attempts.Load())
	})

	t.Run("retry on 5xx then succeed", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			count := attempts.Add(1)
			if count <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer server.Close()

		resp, err := doWithRetry(t.Context(), server.Client(), func() (*http.Request, error) {
			return http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
		})
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, int32(3), attempts.Load())
	})

	t.Run("exhaust all retries on 5xx", func(t *testing.T) {
		var attempts atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer server.Close()

		_, err := doWithRetry(t.Context(), server.Client(), func() (*http.Request, error) {
			return http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, http.NoBody)
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "all")
		assert.Equal(t, int32(retryAttempts+1), attempts.Load())
	})
}

func TestResolveLocation(t *testing.T) {
	t.Run("absolute URL returned as-is", func(t *testing.T) {
		result, err := resolveLocation("http://example.com/v2/repo/blobs/uploads/", "http://other.com/v2/repo/blobs/uploads/uuid")
		require.NoError(t, err)
		assert.Equal(t, "http://other.com/v2/repo/blobs/uploads/uuid", result)
	})

	t.Run("relative URL resolved against base", func(t *testing.T) {
		result, err := resolveLocation("http://example.com/v2/repo/blobs/uploads/", "/v2/repo/blobs/uploads/uuid")
		require.NoError(t, err)
		assert.Equal(t, "http://example.com/v2/repo/blobs/uploads/uuid", result)
	})

	t.Run("empty location returns error", func(t *testing.T) {
		_, err := resolveLocation("http://example.com/base", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty")
	})
}

func TestAppendDigestParam(t *testing.T) {
	t.Run("appends digest to URL", func(t *testing.T) {
		digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
		result, err := appendDigestParam("http://example.com/v2/repo/blobs/uploads/uuid", digest)
		require.NoError(t, err)
		assert.Contains(t, result, "digest=sha256%3Aabc123")
	})

	t.Run("preserves existing query params", func(t *testing.T) {
		digest := v1.Hash{Algorithm: "sha256", Hex: "abc123"}
		result, err := appendDigestParam("http://example.com/path?existing=value", digest)
		require.NoError(t, err)
		assert.Contains(t, result, "existing=value")
		assert.Contains(t, result, "digest=sha256%3Aabc123")
	})
}
