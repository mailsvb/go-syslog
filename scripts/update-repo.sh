#!/usr/bin/env bash
# Builds the zypper (rpm-md) repository that is published on GitHub Pages.
#
# usage: update-repo.sh <site-dir> <base-url> <keep> <new.rpm>...
#
#   site-dir  directory that holds the repository (existing RPMs in x86_64/)
#   base-url  public URL of the repository, e.g. https://owner.github.io/go-syslog/
#   keep      number of RPMs to keep (the newest by version)
#
# If SIGN_KEY_ID is set, repodata/repomd.xml is signed with that GPG key
# (GPG_PASSPHRASE optional) and the public key is published next to it.
set -euo pipefail

site=$1
base_url=${2%/}/
keep=$3
shift 3

pkgdir="$site/x86_64"
mkdir -p "$pkgdir"
cp -f "$@" "$pkgdir/"

# Keep only the newest $keep packages (version sort).
mapfile -t all < <(find "$pkgdir" -maxdepth 1 -name 'go-syslog-*.rpm' -printf '%f\n' | sort -V)
if (( ${#all[@]} > keep )); then
    for f in "${all[@]:0:${#all[@]}-keep}"; do
        echo "pruning $f"
        rm -f -- "$pkgdir/$f"
    done
fi

rm -rf -- "$site/repodata"
createrepo_c --quiet "$site"

gpgcheck=0
if [[ -n "${SIGN_KEY_ID:-}" ]]; then
    gpg_args=(--batch --yes --pinentry-mode loopback --local-user "$SIGN_KEY_ID")
    if [[ -n "${GPG_PASSPHRASE:-}" ]]; then
        gpg_args+=(--passphrase "$GPG_PASSPHRASE")
    fi
    gpg "${gpg_args[@]}" --armor --detach-sign "$site/repodata/repomd.xml"
    gpg --batch --yes --armor --export "$SIGN_KEY_ID" > "$site/repodata/repomd.xml.key"
    cp "$site/repodata/repomd.xml.key" "$site/gpg-pubkey.asc"
    gpgcheck=1
fi

cat > "$site/go-syslog.repo" <<EOF
[go-syslog]
name=go-syslog
baseurl=${base_url}
type=rpm-md
enabled=1
autorefresh=1
gpgcheck=${gpgcheck}
repo_gpgcheck=${gpgcheck}
EOF
if (( gpgcheck )); then
    echo "gpgkey=${base_url}repodata/repomd.xml.key" >> "$site/go-syslog.repo"
fi

# GitHub Pages must serve the files as they are.
touch "$site/.nojekyll"

{
    echo '<!doctype html><html><head><meta charset="utf-8"><title>go-syslog repository</title></head><body>'
    echo '<h1>go-syslog zypper repository</h1>'
    echo "<pre>zypper addrepo ${base_url}go-syslog.repo
zypper refresh
zypper install go-syslog</pre><h2>Packages</h2><ul>"
    find "$pkgdir" -maxdepth 1 -name 'go-syslog-*.rpm' -printf '%f\n' | sort -rV | while read -r f; do
        echo "<li><a href=\"x86_64/$f\">$f</a></li>"
    done
    echo '</ul></body></html>'
} > "$site/index.html"

echo "repository contents:"
find "$site" -type f -not -path '*/.git/*' | sort
