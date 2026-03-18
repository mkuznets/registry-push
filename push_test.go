package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildManifest(t *testing.T) {
	t.Run("produces valid OCI manifest", func(t *testing.T) {
		img := buildTestImage(t)

		manifestBytes, mediaType, err := buildManifest(img)
		require.NoError(t, err)
		assert.Equal(t, "application/vnd.oci.image.manifest.v1+json", mediaType)

		var m ociManifest
		require.NoError(t, json.Unmarshal(manifestBytes, &m))

		assert.Equal(t, 2, m.SchemaVersion)
		assert.Equal(t, "application/vnd.oci.image.manifest.v1+json", m.MediaType)
		assert.Equal(t, "application/vnd.oci.image.config.v1+json", m.Config.MediaType)
		assert.NotEmpty(t, m.Config.Digest)
		assert.Greater(t, m.Config.Size, int64(0))
		assert.Len(t, m.Layers, 1)
		assert.NotEmpty(t, m.Layers[0].Digest)
		assert.Greater(t, m.Layers[0].Size, int64(0))
	})
}

func TestPushManifest(t *testing.T) {
	t.Run("PUT manifest to correct URL", func(t *testing.T) {
		var receivedBody []byte
		var receivedContentType string
		var receivedPath string

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			receivedPath = r.URL.Path
			receivedContentType = r.Header.Get("Content-Type")
			receivedBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
		}))
		defer server.Close()

		cred := Credentials{Username: "user", Password: "pass"}
		manifestBytes := []byte(`{"schemaVersion":2}`)
		err := pushManifest(t.Context(), server.Client(), server.URL+"/v2/myrepo", cred, "v1.0", manifestBytes, "application/vnd.oci.image.manifest.v1+json")
		require.NoError(t, err)

		assert.Equal(t, "/v2/myrepo/manifests/v1.0", receivedPath)
		assert.Equal(t, "application/vnd.oci.image.manifest.v1+json", receivedContentType)
		assert.Equal(t, manifestBytes, receivedBody)
	})
}

func TestPushImageEndToEnd(t *testing.T) {
	t.Run("full push flow", func(t *testing.T) {
		img := buildTestImage(t)

		layers, err := img.Layers()
		require.NoError(t, err)
		require.Len(t, layers, 1)

		layerDigest, err := layers[0].Digest()
		require.NoError(t, err)

		configRaw, err := img.RawConfigFile()
		require.NoError(t, err)
		configDigest, _, err := v1.SHA256(strings.NewReader(string(configRaw)))
		require.NoError(t, err)

		var mu sync.Mutex
		receivedBlobs := make(map[string]*bytes.Buffer)
		var receivedManifest []byte
		var receivedManifestPath string
		var uploadCounter int

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			assert.True(t, ok, "basic auth should be present")
			assert.Equal(t, "testuser", user)
			assert.Equal(t, "testpass", pass)

			path := r.URL.Path

			switch r.Method {
			case http.MethodHead:
				w.WriteHeader(http.StatusNotFound)

			case http.MethodPost:
				mu.Lock()
				uploadCounter++
				uuid := fmt.Sprintf("uuid-%d", uploadCounter)
				mu.Unlock()
				w.Header().Set("Location", fmt.Sprintf("/v2/test/repo/blobs/uploads/%s", uuid))
				w.WriteHeader(http.StatusAccepted)

			case http.MethodPatch:
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				buf, exists := receivedBlobs[path]
				if !exists {
					buf = &bytes.Buffer{}
					receivedBlobs[path] = buf
				}
				buf.Write(body)
				mu.Unlock()
				w.Header().Set("Location", r.URL.RequestURI())
				w.WriteHeader(http.StatusAccepted)

			case http.MethodPut:
				if strings.Contains(path, "/manifests/") {
					mu.Lock()
					receivedManifest, _ = io.ReadAll(r.Body)
					receivedManifestPath = path
					mu.Unlock()
					w.WriteHeader(http.StatusCreated)
				} else {
					w.WriteHeader(http.StatusCreated)
				}

			default:
				t.Errorf("unexpected request: %s %s", r.Method, path)
			}
		}))
		defer server.Close()

		dest := Destination{
			Host:       strings.TrimPrefix(server.URL, "http://"),
			Repository: "test/repo",
			Tag:        "v1.0",
		}

		opts := &Options{
			ChunkSize:   0,
			Concurrency: 2,
			GzipLevel:   6,
			Insecure:    true,
			Username:    "testuser",
			Password:    "testpass",
		}
		opts.Args.Source = "unused"

		err = pushImageWithSource(t.Context(), opts, dest, "http", img)
		require.NoError(t, err)

		mu.Lock()
		defer mu.Unlock()

		assert.Equal(t, "/v2/test/repo/manifests/v1.0", receivedManifestPath)
		assert.NotEmpty(t, receivedManifest)

		var m ociManifest
		require.NoError(t, json.Unmarshal(receivedManifest, &m))
		assert.Equal(t, 2, m.SchemaVersion)
		assert.Equal(t, configDigest.String(), m.Config.Digest)
		assert.Len(t, m.Layers, 1)
		assert.Equal(t, layerDigest.String(), m.Layers[0].Digest)

		blobCount := 0
		for _, buf := range receivedBlobs {
			assert.Greater(t, buf.Len(), 0)
			blobCount++
		}
		assert.Equal(t, 2, blobCount)
	})
}

func buildTestImage(t *testing.T) v1.Image {
	t.Helper()

	layer, err := partial.UncompressedToLayer(&staticUncompressedLayer{
		content:   bytes.Repeat([]byte("hello world "), 100),
		mediaType: types.OCILayer,
	})
	require.NoError(t, err)

	img, err := mutate.AppendLayers(empty.Image, layer)
	require.NoError(t, err)

	return img
}

type staticUncompressedLayer struct {
	content   []byte
	mediaType types.MediaType
}

func (l *staticUncompressedLayer) Uncompressed() (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(l.content)), nil
}

func (l *staticUncompressedLayer) DiffID() (v1.Hash, error) {
	h, _, err := v1.SHA256(bytes.NewReader(l.content))
	return h, err
}

func (l *staticUncompressedLayer) MediaType() (types.MediaType, error) {
	return l.mediaType, nil
}
