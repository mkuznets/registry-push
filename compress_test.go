package main

import (
	"compress/gzip"
	"io"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessImage_PassthroughPreservesLayers(t *testing.T) {
	img, err := random.Image(1024, 3)
	require.NoError(t, err)

	result, err := ProcessImage(img, false, 9)
	require.NoError(t, err)

	origLayers, err := img.Layers()
	require.NoError(t, err)
	resultLayers, err := result.Layers()
	require.NoError(t, err)
	require.Len(t, resultLayers, len(origLayers))

	for i := range origLayers {
		origDigest, err := origLayers[i].Digest()
		require.NoError(t, err)
		resultDigest, err := resultLayers[i].Digest()
		require.NoError(t, err)
		assert.Equal(t, origDigest, resultDigest)

		origSize, err := origLayers[i].Size()
		require.NoError(t, err)
		resultSize, err := resultLayers[i].Size()
		require.NoError(t, err)
		assert.Equal(t, origSize, resultSize)

		origRC, err := origLayers[i].Compressed()
		require.NoError(t, err)
		origData, err := io.ReadAll(origRC)
		require.NoError(t, err)
		require.NoError(t, origRC.Close())

		resultRC, err := resultLayers[i].Compressed()
		require.NoError(t, err)
		resultData, err := io.ReadAll(resultRC)
		require.NoError(t, err)
		require.NoError(t, resultRC.Close())

		assert.Equal(t, origData, resultData)
	}
}

func TestProcessImage_RecompressProducesValidGzip(t *testing.T) {
	img, err := random.Image(1024, 2)
	require.NoError(t, err)

	result, err := ProcessImage(img, true, 6)
	require.NoError(t, err)

	resultLayers, err := result.Layers()
	require.NoError(t, err)
	require.Len(t, resultLayers, 2)

	origLayers, err := img.Layers()
	require.NoError(t, err)

	for i, layer := range resultLayers {
		rc, err := layer.Compressed()
		require.NoError(t, err)
		gr, err := gzip.NewReader(rc)
		require.NoError(t, err)
		decompressed, err := io.ReadAll(gr)
		require.NoError(t, err)
		require.NoError(t, gr.Close())
		require.NoError(t, rc.Close())

		origRC, err := origLayers[i].Uncompressed()
		require.NoError(t, err)
		origUncompressed, err := io.ReadAll(origRC)
		require.NoError(t, err)
		require.NoError(t, origRC.Close())

		assert.Equal(t, origUncompressed, decompressed)

		origDiffID, err := origLayers[i].DiffID()
		require.NoError(t, err)
		resultDiffID, err := layer.DiffID()
		require.NoError(t, err)
		assert.Equal(t, origDiffID, resultDiffID)

		size, err := layer.Size()
		require.NoError(t, err)
		assert.Greater(t, size, int64(0))

		digest, err := layer.Digest()
		require.NoError(t, err)
		assert.NotEqual(t, v1.Hash{}, digest)
	}
}

func TestProcessImage_RecompressPreservesConfig(t *testing.T) {
	img, err := random.Image(1024, 2)
	require.NoError(t, err)

	result, err := ProcessImage(img, true, 9)
	require.NoError(t, err)

	origCfg, err := img.ConfigFile()
	require.NoError(t, err)
	resultCfg, err := result.ConfigFile()
	require.NoError(t, err)

	assert.Equal(t, origCfg.Architecture, resultCfg.Architecture)
	assert.Equal(t, origCfg.OS, resultCfg.OS)
	require.Len(t, resultCfg.RootFS.DiffIDs, len(origCfg.RootFS.DiffIDs))
	for i := range origCfg.RootFS.DiffIDs {
		assert.Equal(t, origCfg.RootFS.DiffIDs[i], resultCfg.RootFS.DiffIDs[i])
	}
}

func TestProcessImage_CompressedReadableMultipleTimes(t *testing.T) {
	img, err := random.Image(1024, 1)
	require.NoError(t, err)

	result, err := ProcessImage(img, true, 5)
	require.NoError(t, err)

	layers, err := result.Layers()
	require.NoError(t, err)
	require.Len(t, layers, 1)

	var firstRead []byte
	for i := range 3 {
		rc, err := layers[0].Compressed()
		require.NoError(t, err)
		data, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		assert.Greater(t, len(data), 0)
		if i == 0 {
			firstRead = data
		} else {
			assert.Equal(t, firstRead, data)
		}
	}
}
