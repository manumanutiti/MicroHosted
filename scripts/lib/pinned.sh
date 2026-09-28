# Sourced, not run: checks a downloaded file against scripts/checksums.sha256.
#
#   source "$(dirname "${BASH_SOURCE[0]}")/lib/pinned.sh"
#   verify_pinned kernel/x86_64/vmlinux-6.1.102 path/to/vmlinux || exit 1
#
# A key with no entry is refused like a mismatch: an unpinned download is
# exactly what this is here to stop.

PINNED_FILE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/checksums.sha256"

# pinned_sha256 KEY — the pinned hash of KEY, or nothing.
pinned_sha256() {
  awk -v k="$1" '$1 !~ /^#/ && $2 == k { print $1 }' "$PINNED_FILE"
}

# verify_pinned KEY FILE — 0 when FILE's SHA-256 is KEY's pinned hash. FILE
# may be root's (the store): it is then hashed with sudo.
verify_pinned() {
  local key="$1" file="$2" want got
  want="$(pinned_sha256 "$key")"
  if [[ -z "$want" ]]; then
    echo "ERROR: no pinned SHA-256 for $key in $PINNED_FILE." >&2
    echo "       A new version is added there by hand, after checking it against its" >&2
    echo "       publisher (the file's header says how for each source)." >&2
    return 1
  fi
  if [[ -r "$file" ]]; then
    got="$(sha256sum "$file" | cut -d' ' -f1)"
  else
    got="$(sudo sha256sum "$file" | cut -d' ' -f1)"
  fi
  if [[ "$got" != "$want" ]]; then
    echo "ERROR: $file is not the pinned $key:" >&2
    echo "       want sha256 $want" >&2
    echo "       got  sha256 $got" >&2
    return 1
  fi
  echo "  sha256 verified: $key"
}
