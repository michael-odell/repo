# Release builds require goreleaser: `brew install goreleaser`.

# List available recipes
default:
    @just --list

# Local development build (plain go build, matches README)
build:
    go build -o repo ./cmd/repo

# Run the same checks as CI (.github/workflows/ci.yml)
check:
    go vet ./...
    go build ./...
    go test ./...

# Multi-platform release build via goreleaser, without publishing (dist/) — for local testing
release-build:
    goreleaser release --snapshot --clean --skip=publish

# Real release build + publish to GitHub Releases; requires a vX.Y.Z tag on HEAD (also run by CI, see release.yml)
release:
    goreleaser release --clean

# Tag a new minor version series off the latest tag (e.g. v0.2.5 -> v0.3.0)
tag-minor: _clean-worktree
    #!/usr/bin/env bash
    set -euo pipefail
    latest=$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sort -V | tail -n1)
    latest=${latest:-v0.0.0}
    IFS='.' read -r major minor _patch <<< "${latest#v}"
    new="v${major}.$((minor + 1)).0"
    just _create-tag "${latest}" "${new}"

# Tag a new major version series off the latest tag (e.g. v0.2.5 -> v1.0.0)
tag-major: _clean-worktree
    #!/usr/bin/env bash
    set -euo pipefail
    latest=$(git tag -l 'v[0-9]*.[0-9]*.[0-9]*' | sort -V | tail -n1)
    latest=${latest:-v0.0.0}
    IFS='.' read -r major _minor _patch <<< "${latest#v}"
    new="v$((major + 1)).0.0"
    just _create-tag "${latest}" "${new}"

# Internal: create an annotated tag and offer to push it
_create-tag latest new:
    #!/usr/bin/env bash
    set -euo pipefail
    echo "Latest tag: {{ latest }}"
    echo "New tag:    {{ new }}"
    git tag -a "{{ new }}" -m "{{ new }}"
    echo "Created {{ new }}."
    read -r -p "Push {{ new }} to origin now? [y/N] " ans
    if [[ "${ans}" =~ ^[Yy]$ ]]; then
        git push origin "{{ new }}"
    else
        echo "Skipped push. Run: git push origin {{ new }}"
    fi

# Internal: refuse to tag with uncommitted changes
_clean-worktree:
    #!/usr/bin/env bash
    set -euo pipefail
    if [ -n "$(git status --porcelain)" ]; then
        echo "error: working tree is dirty; commit or stash changes before tagging" >&2
        exit 1
    fi
