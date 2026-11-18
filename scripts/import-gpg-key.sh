#!/usr/bin/env bash
# Imports the package signing key from the PACKAGE_SIGNING_KEY environment
# variable (the same secret name as in mailsvb/go-fs).
#
# usage: PACKAGE_SIGNING_KEY=... import-gpg-key.sh <key-file>
#
# Accepts the PGP private key ASCII-armored (gpg --armor --export-secret-keys),
# with CRLF line ends or with its line breaks written as a literal \n, or base64
# encoded (armored or binary export). Writes the decoded key to <key-file> (for
# nfpm), imports it into the gpg keyring and prints the long key id on stdout.
# A passphrase, if the key has one, comes from PACKAGE_SIGNING_PASSPHRASE and
# is used later by nfpm and update-repo.sh. Diagnostics go to stderr and never
# contain key material.
set -euo pipefail

out=$1
key=${PACKAGE_SIGNING_KEY:-}
umask 077

fail() {
    echo "::error::$1" >&2
    echo "PACKAGE_SIGNING_KEY: ${#key} characters, $(printf '%s\n' "$key" | wc -l | tr -d ' ') lines" >&2
    for h in "PGP PRIVATE KEY BLOCK" "PGP PUBLIC KEY BLOCK" "OPENSSH PRIVATE KEY" "RSA PRIVATE KEY" "PRIVATE KEY"; do
        if grep -q -- "-----BEGIN $h-----" <<<"$key"; then
            echo "PACKAGE_SIGNING_KEY contains the header: -----BEGIN $h-----" >&2
            break
        fi
    done
    echo "Expected the output of: gpg --armor --export-secret-keys <KEYID>" >&2
    echo "(with its line breaks), or that output encoded with base64." >&2
    exit 1
}

# A 32-byte key in base64 (ed25519 seed, e.g. go-fs's GOFS_SIGNING_KEY) is a
# common mix-up; say so instead of a generic error.
compact=$(printf '%s' "$key" | tr -d ' \t\r\n')
if [[ ${#compact} -eq 44 && $compact =~ ^[A-Za-z0-9+/]{43}=$ ]]; then
    fail "PACKAGE_SIGNING_KEY holds a 32-byte base64 key (like an ed25519 update signing key such as GOFS_SIGNING_KEY), not a PGP private key; use the value of go-fs's PACKAGE_SIGNING_KEY"
fi

if grep -q -- '-----BEGIN PGP PRIVATE KEY BLOCK-----' <<<"$key"; then
    printf '%s\n' "$key" | tr -d '\r' >"$out"
    # pasted as one line with literal \n between the lines
    if [[ $(wc -l <"$out") -le 1 ]] && grep -qF -- '\n' "$out"; then
        printf '%b\n' "$(tr -d '\n' <"$out")" | sed '/^$/{N;/^\n$/D;}' >"$out.tmp" && mv "$out.tmp" "$out"
    fi
    if [[ $(wc -l <"$out") -lt 3 ]]; then
        fail "PACKAGE_SIGNING_KEY is armored but its line breaks are missing; paste the key again exactly as exported"
    fi
elif printf '%s' "$compact" | base64 -d >"$out" 2>/dev/null && [[ -s "$out" ]]; then
    echo "PACKAGE_SIGNING_KEY is base64 encoded; decoded it" >&2
else
    fail "PACKAGE_SIGNING_KEY is neither an armored PGP private key nor base64"
fi

# Check the content before importing it: it must hold a secret key.
listing=$(gpg --batch --with-colons --show-keys "$out" 2>/dev/null || true)
grep -Eq '^(sec|pub):' <<<"$listing" ||
    fail "PACKAGE_SIGNING_KEY does not contain valid OpenPGP data"
grep -q '^sec:' <<<"$listing" ||
    fail "PACKAGE_SIGNING_KEY holds only a public key; export it with --export-secret-keys"

gpg --batch --import "$out" >&2
awk -F: '/^sec/ {print $5; exit}' <<<"$listing"
