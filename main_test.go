package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDestination(t *testing.T) {
	t.Run("host/repo:tag", func(t *testing.T) {
		dest, err := ParseDestination("registry.example.com/myapp:v1.0")
		require.NoError(t, err)
		assert.Equal(t, "registry.example.com", dest.Host)
		assert.Equal(t, "myapp", dest.Repository)
		assert.Equal(t, "v1.0", dest.Tag)
	})

	t.Run("host/nested/repo:tag", func(t *testing.T) {
		dest, err := ParseDestination("ghcr.io/org/repo:latest")
		require.NoError(t, err)
		assert.Equal(t, "ghcr.io", dest.Host)
		assert.Equal(t, "org/repo", dest.Repository)
		assert.Equal(t, "latest", dest.Tag)
	})

	t.Run("defaults to latest tag", func(t *testing.T) {
		dest, err := ParseDestination("registry.example.com/myapp")
		require.NoError(t, err)
		assert.Equal(t, "registry.example.com", dest.Host)
		assert.Equal(t, "myapp", dest.Repository)
		assert.Equal(t, "latest", dest.Tag)
	})

	t.Run("host with port and tag", func(t *testing.T) {
		dest, err := ParseDestination("localhost:5000/myapp:v2")
		require.NoError(t, err)
		assert.Equal(t, "localhost:5000", dest.Host)
		assert.Equal(t, "myapp", dest.Repository)
		assert.Equal(t, "v2", dest.Tag)
	})

	t.Run("host with port, no tag", func(t *testing.T) {
		dest, err := ParseDestination("localhost:5000/myapp")
		require.NoError(t, err)
		assert.Equal(t, "localhost:5000", dest.Host)
		assert.Equal(t, "myapp", dest.Repository)
		assert.Equal(t, "latest", dest.Tag)
	})

	t.Run("deeply nested repository", func(t *testing.T) {
		dest, err := ParseDestination("docker.io/library/nginx:1.27")
		require.NoError(t, err)
		assert.Equal(t, "docker.io", dest.Host)
		assert.Equal(t, "library/nginx", dest.Repository)
		assert.Equal(t, "1.27", dest.Tag)
	})

	t.Run("empty string", func(t *testing.T) {
		_, err := ParseDestination("")
		assert.Error(t, err)
	})

	t.Run("no host, just name", func(t *testing.T) {
		_, err := ParseDestination("myapp")
		assert.Error(t, err)
	})

	t.Run("empty host", func(t *testing.T) {
		_, err := ParseDestination("/myapp:v1")
		assert.Error(t, err)
	})

	t.Run("empty repository", func(t *testing.T) {
		_, err := ParseDestination("registry.example.com/")
		assert.Error(t, err)
	})
}
