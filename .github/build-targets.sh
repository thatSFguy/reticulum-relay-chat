#!/usr/bin/env bash
# Cross-compile rrc-hub for every shipped target into dist/.
#
# Shared by the build and release jobs so a release cannot ship a target
# CI never proved, and so the release does not have to round-trip
# binaries through artifact storage (an account-wide quota that an
# unrelated repo can exhaust).
#
# Usage: build-targets.sh <version>
set -euo pipefail

version="${1:-dev}"
mkdir -p dist

for target in ${TARGETS}; do
    IFS=/ read -r goos goarch goarm name <<< "${target}"
    ext=""
    [ "${goos}" = "windows" ] && ext=".exe"

    echo "==> ${name} (${goos}/${goarch}${goarm:+v${goarm}})"
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" GOARM="${goarm}" \
        go build -trimpath \
            -ldflags "-s -w -X main.version=${version}" \
            -o "dist/rrc-hub-${name}${ext}" ./cmd/rrc-hub
done

ls -la dist/
