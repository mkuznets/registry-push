package main

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/layout"
)

type SourceType int

const (
	SourceDaemon SourceType = iota
	SourceOCI
)

func ClassifySource(ref string) (srcType SourceType, cleanRef string) {
	if strings.HasPrefix(ref, "oci:") {
		return SourceOCI, strings.TrimPrefix(ref, "oci:")
	}
	return SourceDaemon, ref
}

func ResolveSource(ref string) (v1.Image, error) {
	srcType, cleanRef := ClassifySource(ref)

	switch srcType {
	case SourceDaemon:
		daemonRef, err := name.ParseReference(cleanRef)
		if err != nil {
			return nil, fmt.Errorf("parsing daemon reference %q: %w", cleanRef, err)
		}
		return daemon.Image(daemonRef)

	case SourceOCI:
		idx, err := layout.ImageIndexFromPath(cleanRef)
		if err != nil {
			return nil, fmt.Errorf("reading OCI layout from %q: %w", cleanRef, err)
		}
		idxManifest, err := idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("reading OCI index manifest: %w", err)
		}
		if len(idxManifest.Manifests) == 0 {
			return nil, fmt.Errorf("OCI layout at %q has no manifests", cleanRef)
		}
		return idx.Image(idxManifest.Manifests[0].Digest)

	default:
		return nil, fmt.Errorf("unknown source type for %q", ref)
	}
}
