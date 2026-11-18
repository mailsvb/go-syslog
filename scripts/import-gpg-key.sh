#!/usr/bin/env bash
# Imports the release signing key from the GPG_PRIVATE_KEY environment variable.
#
# usage: GPG_PRIVATE_KEY=... import-gpg-key.sh <key-file>
#
# Accepts the private key ASCII-armored (gpg --armor --export-secret-keys) or
# base64 encoded (gpg --export-secret-keys | base64, armored or binary).
# Writes the decoded key to <key-file> (for nfpm), imports it into the gpg
# keyring and prints the long key id on stdout. Diagnostics go to stderr and
# never contain key material.
set -euo pipefail

out=$1
key=${GPG_PRIVATE_KEY:-}
umask 077

fail() {
    echo "::error::$1" >&2
    echo "GPG_PRIVATE_KEY: ${#key} characters, $(printf '%s\n' "$key" | wc -l | tr -d ' ') lines" >&2
    for h in "PGP PRIVATE KEY BLOCK" "PGP PUBLIC KEY BLOCK" "OPENSSH PRIVATE KEY" "RSA PRIVATE KEY" "PRIVATE KEY"; do
        if grep -q -- "-----BEGIN $h-----" <<<"$key"; then
            echo "GPG_PRIVATE_KEY contains the header: -----BEGIN $h-----" >&2
            break
        fi
    done
    echo "Expected the output of: gpg --armor --export-secret-keys <KEYID>" >&2
    echo "(with its line breaks), or that output encoded with base64." >&2
    exit 1
}

if grep -q -- '-----BEGIN PGP PRIVATE KEY BLOCK-----' <<<"$key"; then
    printf '%s\n' "$key" | tr -d '\r' >"$out"
    if [[ $(wc -l <"$out") -lt 3 ]]; then
        fail "GPG_PRIVATE_KEY is armored but its line breaks are missing; paste the key again exactly as exported"
    fi
elif printf '%s' "$key" | tr -d ' \t\r\n' | base64 -d >"$out" 2>/dev/null && [[ -s "$out" ]]; then
    echo "GPG_PRIVATE_KEY is base64 encoded; decoded it" >&2
else
    fail "GPG_PRIVATE_KEY is neither an armored PGP private key nor base64"
fi

# Check the content before importing it: it must hold a secret key.
listing=$(gpg --batch --with-colons --show-keys "$out" 2>/dev/null || true)
grep -Eq '^(sec|pub):' <<<"$listing" ||
    fail "GPG_PRIVATE_KEY does not contain valid OpenPGP data"
grep -q '^sec:' <<<"$listing" ||
    fail "GPG_PRIVATE_KEY holds only a public key; export it with --export-secret-keys"

gpg --batch --import "$out" >&2
awk -F: '/^sec/ {print $5; exit}' <<<"$listing"
