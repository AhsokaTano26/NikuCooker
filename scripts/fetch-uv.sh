#!/bin/sh
#
# Stages a copy of uv for every platform this project ships.
#
# The release archives carry uv so that installing the AI environment is one
# download rather than two, and so that the copy the user runs is the one this
# build was tested against rather than whatever is on their PATH. It is a single
# static binary of about 20 MB, which is the whole reason that trade is
# affordable.
#
# Run by GoReleaser's before-hooks, and harmless to run by hand:
#
#   ./scripts/fetch-uv.sh          # stage into build/uv/
#
# The GOOS/GOARCH → uv asset mapping lives here and nowhere else. A release
# asset name is not a GOOS/GOARCH pair, and spreading that translation across
# a YAML file and a workflow is how one of them ends up wrong.

set -eu

# Pinned. A floating "latest" makes a release unreproducible, and can change
# between the snapshot built on a pull request and the tag that follows it.
UV_VERSION="${UV_VERSION:-0.12.18}"

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
STAGE="${ROOT}/build/uv"
BASE="https://github.com/astral-sh/uv/releases/download/${UV_VERSION}"

# goos goarch asset member
TARGETS="
linux/amd64   uv-x86_64-unknown-linux-gnu.tar.gz   uv
linux/arm64   uv-aarch64-unknown-linux-gnu.tar.gz  uv
darwin/amd64  uv-x86_64-apple-darwin.tar.gz        uv
darwin/arm64  uv-aarch64-apple-darwin.tar.gz       uv
windows/amd64 uv-x86_64-pc-windows-msvc.zip        uv.exe
"

# A checksum verifier that exists on both of the machines this runs on —
# developer laptops and the release runners. macOS has shasum, Linux has
# sha256sum, and assuming either one alone breaks the other.
verify_sha256() {
	file=$1
	expected=$2

	if command -v sha256sum >/dev/null 2>&1; then
		actual=$(sha256sum "$file" | cut -d' ' -f1)
	elif command -v shasum >/dev/null 2>&1; then
		actual=$(shasum -a 256 "$file" | cut -d' ' -f1)
	else
		echo "fetch-uv: no sha256 tool found; refusing to install an unverified binary" >&2
		exit 1
	fi

	if [ "$actual" != "$expected" ]; then
		echo "fetch-uv: checksum mismatch for $file" >&2
		echo "  expected $expected" >&2
		echo "  got      $actual" >&2
		exit 1
	fi
}

echo "$TARGETS" | while read -r target asset member; do
	[ -n "${target:-}" ] || continue

	goos=${target%%/*}
	goarch=${target##*/}
	dest="${STAGE}/${goos}_${goarch}"

	# Already staged. GoReleaser's --clean removes the output directory, not
	# this one, so a second local run does not download 100 MB again.
	if [ -x "${dest}/${member}" ]; then
		echo "  ${goos}/${goarch}: already staged"
		continue
	fi

	echo "  ${goos}/${goarch}: ${asset}"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	curl -fsSL --retry 3 --proto '=https' -o "${tmp}/${asset}" "${BASE}/${asset}"
	curl -fsSL --retry 3 --proto '=https' -o "${tmp}/${asset}.sha256" "${BASE}/${asset}.sha256"

	# The published file is "<hash>  <filename>"; take the hash and ignore the
	# rest, so a change to the second field is not a failure.
	verify_sha256 "${tmp}/${asset}" "$(cut -d' ' -f1 < "${tmp}/${asset}.sha256")"

	mkdir -p "$dest"
	# Everything, then strip the single top-level directory. Naming the member
	# explicitly would mean naming its path inside the archive — which is not
	# the basename, and which is uv's layout to change. The archive also
	# carries uvx; it comes along harmlessly.
	case "$asset" in
	*.zip) unzip -q -j "${tmp}/${asset}" -d "$dest" ;;
	*) tar -xzf "${tmp}/${asset}" -C "$dest" --strip-components=1 ;;
	esac

	# Asserted rather than assumed. uv's archive layout is not a contract, and
	# an empty stage would ship an archive whose uv does not exist — which the
	# user discovers at the moment they click install.
	if [ ! -s "${dest}/${member}" ]; then
		echo "fetch-uv: ${asset} did not contain ${member}" >&2
		exit 1
	fi

	chmod 0755 "${dest}/${member}"
	rm -rf "$tmp"
	trap - EXIT INT TERM

done

echo "fetch-uv: uv ${UV_VERSION} staged in build/uv"
