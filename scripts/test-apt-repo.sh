#!/bin/bash
# Проверка APT-репозитория: собирает его из deb-пакетов одноразовым ключом и ставит
# пакет через apt в чистых контейнерах на архитектуре хоста.
# Использование: scripts/test-apt-repo.sh <каталог с deb> [образ...]
set -euo pipefail
debs=$(realpath "$1")
shift
images=("$@")
[ ${#images[@]} -gt 0 ] || images=(ubuntu:26.04 ubuntu:24.04 debian:13)
here=$(dirname "$(realpath "$0")")

work=$(mktemp -d)
trap 'gpgconf --homedir "$work/gnupg" --kill all; rm -rf "$work"' EXIT
export GNUPGHOME=$work/gnupg GPG_PASSPHRASE=test
mkdir -m 700 "$GNUPGHOME"
gpg --batch --pinentry-mode loopback --passphrase "$GPG_PASSPHRASE" \
  --quick-gen-key "apt test key <test@example.invalid>" ed25519 sign 1d
fpr=$(gpg --with-colons --list-secret-keys | awk -F: '/^fpr/{print $10; exit}')

"$here/build-apt-repo.sh" "$debs" "$work/site" "$fpr"
chmod -R a+rX "$work/site"

for image in "${images[@]}"; do
  echo "== $image"
  docker run --rm --network host -v "$work/site:/repo:ro" "$image" sh -euc '
    echo "deb [signed-by=/repo/fleeting-plugin-yandex.gpg] file:/repo stable main" \
      > /etc/apt/sources.list.d/fleeting-plugin-yandex.list
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq fleeting-plugin-yandex
    command -v fleeting-plugin-yandex
    fleeting-plugin-yandex -version
  '
done
