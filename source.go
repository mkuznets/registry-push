package main

import (
	"fmt"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

type SourceType int

const (
	SourceDaemon SourceType = iota
	SourceOCI
	SourceRemote
)

func ClassifySource(ref string) (srcType SourceType, cleanRef string) {
	if strings.HasPrefix(ref, "daemon://") {
		return SourceDaemon, strings.TrimPrefix(ref, "daemon://")
	}
	if strings.HasPrefix(ref, "oci:") {
		return SourceOCI, strings.TrimPrefix(ref, "oci:")
	}

	idx := strings.Index(ref, "/")
	if idx == -1 {
		return SourceDaemon, ref
	}

	firstSegment := ref[:idx]
	if strings.Contains(firstSegment, ".") || strings.Contains(firstSegment, ":") || firstSegment == "localhost" {
		return SourceRemote, ref
	}

	return SourceDaemon, ref
}

// parseRemoteReference parses a remote image reference string into a name.Reference.
// Handles the special case where "localhost" without a port needs explicit registry
// override, since go-containerregistry only recognizes first-segment registries
// containing "." or ":".
func parseRemoteReference(ref string) (name.Reference, error) {
	if strings.HasPrefix(ref, "localhost/") {
		ref = strings.TrimPrefix(ref, "localhost/")
		return name.ParseReference(ref, name.WithDefaultRegistry("localhost"))
	}
	return name.ParseReference(ref)
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

	case SourceRemote:
		remoteRef, err := parseRemoteReference(cleanRef)
		if err != nil {
			return nil, fmt.Errorf("parsing remote reference %q: %w", cleanRef, err)
		}
		return remote.Image(remoteRef)

	default:
		return nil, fmt.Errorf("unknown source type for %q", ref)
	}
}
