#!/bin/sh
# Installs wassup from a release: downloads the archive for this machine,
# checks it against checksums.txt, puts the binary in a directory of yours
# and says how to get that directory on the PATH when it is not.
#
#   curl -fsSL https://raw.githubusercontent.com/danilopopovikj/wassup/main/install.sh | sh
#
# WASSUP_VERSION picks a version (default: the latest release).
# WASSUP_INSTALL_DIR picks the directory (default: ~/.local/bin).
# Nothing is installed system-wide and nothing needs root.
set -eu

repo="danilopopovikj/wassup"
dir="${WASSUP_INSTALL_DIR:-$HOME/.local/bin}"

fail() {
	echo "install: $*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is needed and was not found"
}

need curl
need tar
need uname

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "no release for $(uname -s); build from source with: go install github.com/$repo/cmd/wassup@latest" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "no release for $(uname -m); build from source with: go install github.com/$repo/cmd/wassup@latest" ;;
esac

version="${WASSUP_VERSION:-}"
if [ -z "$version" ]; then
	# The latest release redirects to its tag.
	version=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed 's|.*/tag/||')
	case "$version" in
	v*) ;;
	*) fail "no release was found; build from source with: go install github.com/$repo/cmd/wassup@latest" ;;
	esac
fi

archive="wassup_${version#v}_${os}_${arch}.tar.gz"
base="https://github.com/$repo/releases/download/$version"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "downloading $archive ($version)"
curl -fsSL -o "$tmp/$archive" "$base/$archive" || fail "could not download $base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "could not download $base/checksums.txt"

want=$(grep " $archive\$" "$tmp/checksums.txt" | cut -d ' ' -f 1)
[ -n "$want" ] || fail "checksums.txt does not list $archive"
if command -v sha256sum >/dev/null 2>&1; then
	got=$(sha256sum "$tmp/$archive" | cut -d ' ' -f 1)
elif command -v shasum >/dev/null 2>&1; then
	got=$(shasum -a 256 "$tmp/$archive" | cut -d ' ' -f 1)
else
	fail "sha256sum or shasum is needed to check the download"
fi
[ "$got" = "$want" ] || fail "the checksum of $archive does not match checksums.txt; nothing was installed"

tar -xzf "$tmp/$archive" -C "$tmp" wassup
mkdir -p "$dir"
mv "$tmp/wassup" "$dir/wassup"
chmod 755 "$dir/wassup"
echo "installed $dir/wassup"

# The binary knows whether its directory is on the PATH and prints the line
# to add to the shell profile when it is not.
"$dir/wassup" version
