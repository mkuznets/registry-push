package main

import (
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifySource(t *testing.T) {
	t.Run("oci prefix with relative path", func(t *testing.T) {
		srcType, ref := ClassifySource("oci:./my-layout")
		assert.Equal(t, SourceOCI, srcType)
		assert.Equal(t, "./my-layout", ref)
	})

	t.Run("oci prefix with absolute path", func(t *testing.T) {
		srcType, ref := ClassifySource("oci:/tmp/layout")
		assert.Equal(t, SourceOCI, srcType)
		assert.Equal(t, "/tmp/layout", ref)
	})

	t.Run("bare name with tag", func(t *testing.T) {
		srcType, ref := ClassifySource("myimage:latest")
		assert.Equal(t, SourceDaemon, srcType)
		assert.Equal(t, "myimage:latest", ref)
	})

	t.Run("bare name without tag", func(t *testing.T) {
		srcType, ref := ClassifySource("myimage")
		assert.Equal(t, SourceDaemon, srcType)
		assert.Equal(t, "myimage", ref)
	})

	t.Run("org/repo", func(t *testing.T) {
		srcType, ref := ClassifySource("org/repo")
		assert.Equal(t, SourceDaemon, srcType)
		assert.Equal(t, "org/repo", ref)
	})

	t.Run("registry-style ref is still daemon", func(t *testing.T) {
		srcType, ref := ClassifySource("docker.io/library/nginx:1.27")
		assert.Equal(t, SourceDaemon, srcType)
		assert.Equal(t, "docker.io/library/nginx:1.27", ref)
	})

	t.Run("ghcr.io ref is still daemon", func(t *testing.T) {
		srcType, ref := ClassifySource("ghcr.io/org/repo:tag")
		assert.Equal(t, SourceDaemon, srcType)
		assert.Equal(t, "ghcr.io/org/repo:tag", ref)
	})

	t.Run("localhost ref is still daemon", func(t *testing.T) {
		srcType, ref := ClassifySource("localhost/myapp:v1")
		assert.Equal(t, SourceDaemon, srcType)
		assert.Equal(t, "localhost/myapp:v1", ref)
	})

	t.Run("localhost with port is still daemon", func(t *testing.T) {
		srcType, ref := ClassifySource("localhost:5000/myapp:v1")
		assert.Equal(t, SourceDaemon, srcType)
		assert.Equal(t, "localhost:5000/myapp:v1", ref)
	})
}

func TestResolveSource_OCILayout(t *testing.T) {
	dir := t.TempDir()

	img, err := random.Image(1024, 2)
	require.NoError(t, err)

	lp, err := layout.Write(dir, empty.Index)
	require.NoError(t, err)
	require.NoError(t, lp.AppendImage(img))

	resolved, err := ResolveSource("oci:" + dir)
	require.NoError(t, err)
	require.NotNil(t, resolved)

	layers, err := resolved.Layers()
	require.NoError(t, err)
	assert.Len(t, layers, 2)

	originalManifest, err := img.Manifest()
	require.NoError(t, err)
	resolvedManifest, err := resolved.Manifest()
	require.NoError(t, err)
	assert.Equal(t, len(originalManifest.Layers), len(resolvedManifest.Layers))
}

func TestResolveSource_OCILayoutNotFound(t *testing.T) {
	_, err := ResolveSource("oci:/nonexistent/path")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reading OCI layout")
}

func TestResolveSource_OCILayoutEmpty(t *testing.T) {
	dir := t.TempDir()

	_, err := layout.Write(dir, empty.Index)
	require.NoError(t, err)

	_, err = ResolveSource("oci:" + dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no manifests")
}

func TestResolveSource_InvalidDaemonRef(t *testing.T) {
	_, err := ResolveSource("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "parsing daemon reference")
}
