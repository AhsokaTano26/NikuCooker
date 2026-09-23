#!/bin/sh
#
# Checks what the release archives actually contain.
#
#   ./scripts/verify-archive.sh dist/
#
# Exists because the two things most likely to be wrong about a release archive
# are both invisible in the configuration that produces it:
#
#   1. The ai/ tree arrives flattened or incomplete. A glob with strip_parent,
#      or an include-list that misses a new data file, produces an archive that
#      looks right in a file listing and cannot import its own package.
#   2. The bundled uv is missing, or is there under the wrong name. {{ .Arch }}
#      is not documented for the archives file list the way {{ .Os }} is, so
#      the assumption is asserted here rather than trusted.
#
# So this compares the whole tree rather than spot-checking filenames. A check
# that reads two names proves two names.

set -eu

DIST="${1:-dist}"
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

if [ ! -d "$DIST" ]; then
	echo "verify-archive: no such directory: $DIST" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

archives=$(find "$DIST" -maxdepth 1 -type f \( -name '*.tar.gz' -o -name '*.zip' \) | sort)
if [ -z "$archives" ]; then
	echo "verify-archive: no archives in $DIST" >&2
	exit 1
fi

failures=0
checked=0

for archive in $archives; do
	name=$(basename "$archive")
	dir="${work}/${name}"
	mkdir -p "$dir"

	case "$name" in
	*.zip) unzip -q "$archive" -d "$dir" ;;
	*) tar -xzf "$archive" -C "$dir" ;;
	esac

	checked=$((checked + 1))
	echo "  ${name}"

	# --- the AI worker's source ------------------------------------------
	if [ ! -f "${dir}/ai/pyproject.toml" ] || [ ! -f "${dir}/ai/uv.lock" ]; then
		echo "    FAIL: no ai/pyproject.toml and ai/uv.lock; nothing can be installed"
		failures=$((failures + 1))
		continue
	fi

	# A virtual environment must never ship: it holds the builder's absolute
	# paths, and landing it on top of the one the user is about to build is how
	# a container's Python disappears. Checked before anything is stripped, so
	# that its absence is asserted rather than assumed.
	forbidden=0
	for skip in .venv __pycache__ .pytest_cache .ruff_cache .mypy_cache; do
		if [ -e "${dir}/ai/${skip}" ]; then
			echo "    FAIL: ai/${skip} was shipped; it must not be"
			failures=$((failures + 1))
			forbidden=1
		fi
	done

	# The rest of the comparison is whole-tree. tests/ is not shipped — the
	# archive is not a development tree — so it is removed from both sides
	# rather than only one, and the caches are removed from the reference
	# because a checkout has them.
	reference="${work}/reference-ai"
	rm -rf "$reference"
	cp -R "${ROOT}/ai" "$reference"
	for skip in .venv __pycache__ .pytest_cache .ruff_cache .mypy_cache tests; do
		rm -rf "${reference}/${skip}" "${dir}/ai/${skip}"
	done

	if [ "$forbidden" -eq 0 ] && ! diff -r -q "$reference" "${dir}/ai" >/dev/null 2>&1; then
		echo "    FAIL: ai/ in the archive differs from the repository:"
		diff -r -q "$reference" "${dir}/ai" 2>&1 | sed 's/^/      /' | head -20
		failures=$((failures + 1))
	fi

	# --- the bundled uv ---------------------------------------------------
	case "$name" in
	*windows*) uv="uv.exe" ;;
	*) uv="uv" ;;
	esac

	if [ ! -f "${dir}/${uv}" ]; then
		echo "    FAIL: no ${uv}; installing the AI environment would need one from PATH"
		failures=$((failures + 1))
	elif [ ! -x "${dir}/${uv}" ]; then
		echo "    FAIL: ${uv} is not executable"
		failures=$((failures + 1))
	else
		# Only the archive built for this machine can run here, and running it
		# is the one check that proves the staged file is a program rather than
		# a correctly-named zero.
		case "$(uname -s)-$(uname -m)" in
		Darwin-arm64)  host=darwin_arm64 ;;
		Darwin-x86_64) host=darwin_amd64 ;;
		Linux-aarch64) host=linux_arm64 ;;
		Linux-x86_64)  host=linux_amd64 ;;
		*)             host= ;;
		esac

		if [ -n "$host" ]; then
			case "$name" in
			*"$host"*)
				if ! "${dir}/${uv}" --version >/dev/null 2>&1; then
					echo "    FAIL: the bundled ${uv} will not run on this machine"
					failures=$((failures + 1))
				fi
				;;
			esac
		fi
	fi

	# --- the documents the README points at --------------------------------
	for doc in README.md INSTALL.md LICENSE; do
		if [ ! -f "${dir}/${doc}" ]; then
			echo "    FAIL: ${doc} is missing; the README points at it from inside the archive"
			failures=$((failures + 1))
		fi
	done
done

echo
if [ "$failures" -gt 0 ]; then
	echo "verify-archive: ${failures} problem(s) across ${checked} archive(s)" >&2
	exit 1
fi
echo "verify-archive: ${checked} archive(s) look right"
