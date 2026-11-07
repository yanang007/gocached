#!/bin/sh
# Install the gocached binary from GitHub releases.
#
#   curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
#
# Environment:
#   VERSION      release to install, e.g. v1.2.3 or 1.2.3 (default: latest)
#   INSTALL_DIR  target directory (default: /usr/local/bin)
#   GITHUB_URL   base URL of the repository or a mirror of its releases
#                (default: https://github.com/yanang007/gocached)
set -eu

REPO_URL=${GITHUB_URL:-https://github.com/yanang007/gocached}
INSTALL_DIR=${INSTALL_DIR:-/usr/local/bin}
VERSION=${VERSION:-latest}

err() {
	echo "install.sh: $*" >&2
	exit 1
}

# download <url> <file>
download() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		err "need curl or wget"
	fi
}

# sha256 <file> prints the hex sha256 of file.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 "$1" | sed 's/^.*= *//'
	else
		err "need sha256sum, shasum or openssl to verify the download"
	fi
}

case $(uname -s) in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) err "unsupported OS: $(uname -s) (download a release archive manually)" ;;
esac

case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) err "unsupported architecture: $(uname -m)" ;;
esac

if [ "$VERSION" = latest ]; then
	# releases/latest redirects to releases/tag/vX.Y.Z; read the tag from it.
	if command -v curl >/dev/null 2>&1; then
		tag_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$REPO_URL/releases/latest")
	else
		tag_url=$(wget -S --spider "$REPO_URL/releases/latest" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r')
	fi
	tag=${tag_url##*/}
	case $tag in
	v*) ;;
	*) err "could not determine the latest release from $REPO_URL/releases/latest; set VERSION" ;;
	esac
else
	case $VERSION in
	v*) tag=$VERSION ;;
	*) tag=v$VERSION ;;
	esac
fi
version=${tag#v}

archive="gocached_${version}_${os}_${arch}.tar.gz"
base="$REPO_URL/releases/download/$tag"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t gocached)
trap 'rm -rf "$tmp"' EXIT
trap 'exit 1' INT TERM

echo "install.sh: downloading gocached $tag for $os/$arch"
download "$base/$archive" "$tmp/$archive"
download "$base/checksums.txt" "$tmp/checksums.txt"

want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || err "$archive is not listed in checksums.txt"
got=$(sha256 "$tmp/$archive")
[ "$want" = "$got" ] || err "checksum mismatch for $archive: want $want, got $got"

tar -xzf "$tmp/$archive" -C "$tmp" gocached

sudo=""
mkdir -p "$INSTALL_DIR" 2>/dev/null || true
if [ ! -w "$INSTALL_DIR" ]; then
	command -v sudo >/dev/null 2>&1 || err "$INSTALL_DIR is not writable; set INSTALL_DIR or run as root"
	sudo=sudo
fi
$sudo mkdir -p "$INSTALL_DIR"
$sudo cp "$tmp/gocached" "$INSTALL_DIR/.gocached.tmp.$$"
$sudo chmod 0755 "$INSTALL_DIR/.gocached.tmp.$$"
$sudo mv -f "$INSTALL_DIR/.gocached.tmp.$$" "$INSTALL_DIR/gocached"

echo "install.sh: installed $INSTALL_DIR/gocached ($tag)"
