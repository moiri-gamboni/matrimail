#!/usr/bin/env bash
#
# Builds ./matrimail with the mautrix version stamped in, which is the only
# thing this adds over `make build`. The framework reports that version in its
# bridge state, so release builds should come from here.
#
# `goolm` selects mautrix-go's pure-Go Olm implementation, so nothing links the
# deprecated libolm C library. CGO stays on for go-sqlite3.
set -euo pipefail

MAUTRIX_VERSION="$(awk '$1 == "maunium.net/go/mautrix" { print $2; exit }' go.mod)"
if [ -z "$MAUTRIX_VERSION" ]; then
	echo "build.sh: could not read the mautrix version from go.mod" >&2
	exit 1
fi

TAG="$(git describe --exact-match --tags 2>/dev/null || echo unknown)"
COMMIT="$(git rev-parse HEAD)"
BUILD_TIME="$(date -Iseconds)"

GO_LDFLAGS="-s -w -X main.Tag=$TAG -X main.Commit=$COMMIT -X main.BuildTime=$BUILD_TIME -X maunium.net/go/mautrix.GoModVersion=$MAUTRIX_VERSION"

go build -tags goolm -ldflags="$GO_LDFLAGS" "$@" ./cmd/matrimail
