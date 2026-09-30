#!/bin/bash
# Собирает подписанный APT-репозиторий для GitHub Pages из deb-пакетов релиза.
# Использование: scripts/build-apt-repo.sh <каталог с deb> <каталог сайта> <отпечаток ключа>
# Парольная фраза ключа — в $GPG_PASSPHRASE.
# Бинарь статический, от версии дистрибутива не зависит — один suite stable на все.
set -euo pipefail
debs=$1 site=$2 key=$3
name=fleeting-plugin-yandex
suite=stable
arches=(amd64 arm64)

sign() { gpg --batch --yes --pinentry-mode loopback --passphrase-fd 3 --local-user "$key" "$@" 3<<<"$GPG_PASSPHRASE"; }

rm -rf "$site"
pool=pool/main
mkdir -p "$site/$pool"
for a in "${arches[@]}"; do
  found=0
  for deb in "$debs"/"$name"_*_"$a".deb; do
    [ -e "$deb" ] || continue
    cp "$deb" "$site/$pool/"
    found=1
  done
  [ "$found" = 1 ] || { echo "no packages for $a" >&2; exit 1; }
  dir=dists/$suite/main/binary-$a
  mkdir -p "$site/$dir"
  (cd "$site" && apt-ftparchive --arch "$a" packages "$pool" > "$dir/Packages")
  gzip -9nk "$site/$dir/Packages"
done

apt-ftparchive \
  -o APT::FTPArchive::Release::Origin="$name" \
  -o APT::FTPArchive::Release::Label="$name" \
  -o APT::FTPArchive::Release::Suite="$suite" \
  -o APT::FTPArchive::Release::Codename="$suite" \
  -o APT::FTPArchive::Release::Architectures="${arches[*]}" \
  -o APT::FTPArchive::Release::Components=main \
  release "$site/dists/$suite" > "$site/dists/$suite/Release"
sign --clearsign -o "$site/dists/$suite/InRelease" "$site/dists/$suite/Release"
sign --armor --detach-sign -o "$site/dists/$suite/Release.gpg" "$site/dists/$suite/Release"

gpg --armor --export "$key" > "$site/$name.asc"
gpg --export "$key" > "$site/$name.gpg"
cat > "$site/index.html" <<HTML
<!doctype html>
<meta charset="utf-8">
<title>$name APT repository</title>
<h1>$name APT repository</h1>
<p>GitLab Runner fleeting plugin for Yandex Compute Cloud. Suite <code>$suite</code>, amd64 and arm64, any Debian or Ubuntu.</p>
<p>See <a href="https://github.com/AlexeySetevoi/yandex-fleeting-plugin#установка">the README</a> for installation.</p>
HTML
touch "$site/.nojekyll"
