#!/usr/bin/env bash
# Bootstrap must work before Go or Bun is installed. Sourcing is read-only.
vpnkit_dependency_path() {
  export PATH="$PWD/.build/tools/bun/bin:$PATH"
}
vpnkit_dependency_error() { printf '%s\n' "$*" >&2; return 10; }
vpnkit_as_root() {
  if command -v sudo >/dev/null; then sudo "$@"
  elif command -v pkexec >/dev/null; then pkexec "$@"
  else vpnkit_dependency_error 'Для установки системных пакетов нужен sudo или Polkit (pkexec).'; fi
}
vpnkit_package_installed() {
  case "$VPNKIT_PACKAGE_MANAGER" in
    apt) [[ $(dpkg-query -W -f='${Status}' "$1" 2>/dev/null) == 'install ok installed' ]] ;;
    pacman) pacman -Q "$1" >/dev/null 2>&1 ;;
    dnf) rpm -q "$1" >/dev/null 2>&1 ;;
  esac
}
vpnkit_package_available() {
  vpnkit_package_installed "$1" && return 0
  case "$VPNKIT_PACKAGE_MANAGER" in
    apt) apt-cache show "$1" >/dev/null 2>&1 ;;
    pacman) pacman -Si "$1" >/dev/null 2>&1 ;;
    dnf) dnf -q info --available "$1" >/dev/null 2>&1 ;;
  esac
}
vpnkit_choose_package() {
  local candidate
  for candidate in "$@"; do
    if vpnkit_package_available "$candidate"; then printf '%s\n' "$candidate"; return; fi
  done
  vpnkit_dependency_error "Пакет не найден в системных репозиториях: $*"
}
vpnkit_go_ready() {
  local version
  version=$(go version 2>/dev/null) || return 1
  [[ "$version" =~ go([0-9]+)\.([0-9]+) ]] &&
    (( BASH_REMATCH[1] > 1 || (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] >= 22) ))
}
vpnkit_bun_ready() {
  local version
  version=$(bun --version 2>/dev/null) || return 1
  [[ "$version" =~ ^([0-9]+)\.([0-9]+) ]] &&
    (( BASH_REMATCH[1] > 1 || (BASH_REMATCH[1] == 1 && BASH_REMATCH[2] >= 3) ))
}
vpnkit_install_bun() (
  set -Eeuo pipefail
  local_tmp=$(mktemp -d)
  trap 'rm -rf -- "$local_tmp"' EXIT
  printf 'Устанавливаем Bun…\n'
  curl -fsSL --retry 3 --connect-timeout 20 --max-time 120 https://api.github.com/repos/oven-sh/bun/releases/latest -o "$local_tmp/release.json"
  version=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["tag_name"])' "$local_tmp/release.json")
  [[ "$version" =~ ^bun-v[0-9]+\.[0-9]+\.[0-9]+$ ]] || exit 10
  url="https://github.com/oven-sh/bun/releases/download/$version"
  # Baseline build also supports x86-64 CPUs without AVX2.
  asset=bun-linux-x64-baseline.zip
  curl -fsSL --retry 3 --connect-timeout 20 --max-time 300 "$url/$asset" -o "$local_tmp/$asset"
  curl -fsSL --retry 3 --connect-timeout 20 --max-time 120 "$url/SHASUMS256.txt" -o "$local_tmp/checksums"
  (cd "$local_tmp"; awk -v name="$asset" '$2 == name || $2 == "*" name {print}' checksums > selected.sha256; [[ -s selected.sha256 ]]; sha256sum -c selected.sha256)
  unzip -p "$local_tmp/$asset" bun-linux-x64-baseline/bun > "$local_tmp/bun"
  chmod 700 "$local_tmp/bun"
  "$local_tmp/bun" --version
  install -d -m 700 .build/tools/bun/bin
  install -m 755 "$local_tmp/bun" .build/tools/bun/bin/bun
)
vpnkit_install_dependencies() {
  [[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || { vpnkit_dependency_error 'Пока поддерживается Linux x86-64.'; return 10; }
  local ID='' ID_LIKE='' VERSION_ID=''
  . /etc/os-release
  local go_package nm_package ip_package ping_package libs docker_package compose_package buildx_package
  case " $ID $ID_LIKE " in
    *' arch '*|*' cachyos '*) VPNKIT_PACKAGE_MANAGER=pacman; go_package=go; nm_package=networkmanager; ip_package=iproute2; ping_package=iputils; libs=gcc-libs ;;
    *' ubuntu '*|*' debian '*)
      VPNKIT_PACKAGE_MANAGER=apt; go_package=golang-go; nm_package=network-manager; ip_package=iproute2; ping_package=iputils-ping; libs=libstdc++6 ;;
    *' fedora '*) VPNKIT_PACKAGE_MANAGER=dnf; go_package=golang; nm_package=NetworkManager; ip_package=iproute; ping_package=iputils; libs=libstdc++ ;;
    *) vpnkit_dependency_error 'Автоустановка поддерживает Ubuntu/Debian, Fedora и Arch/CachyOS.'; return 10 ;;
  esac
  if [[ $VPNKIT_PACKAGE_MANAGER == apt ]] && ! docker compose version >/dev/null 2>&1; then
    vpnkit_as_root apt-get update || return
  fi
  local -a packages=()
  local command package
  for command in git curl python3 unzip tar sudo flock timeout nmcli ip ping openvpn; do
    case "$command" in
      nmcli) package=$nm_package ;; ip) package=$ip_package ;; ping) package=$ping_package ;;
      flock) package=util-linux ;; timeout) package=coreutils ;;
      python3) if [[ $VPNKIT_PACKAGE_MANAGER == pacman ]]; then package=python; else package=python3; fi ;;
      *) package=$command ;;
    esac
    command -v "$command" >/dev/null || packages+=("$package")
  done
  vpnkit_go_ready || packages+=("$go_package")
  for package in ca-certificates "$libs"; do vpnkit_package_installed "$package" || packages+=("$package"); done
  if [[ ! -f /usr/lib/NetworkManager/VPN/nm-openvpn-service.name && ! -f /usr/lib64/NetworkManager/VPN/nm-openvpn-service.name ]]; then
    case "$VPNKIT_PACKAGE_MANAGER" in
      apt) packages+=(network-manager-openvpn) ;;
      pacman) package=$(vpnkit_choose_package networkmanager-vpn-plugin-openvpn networkmanager-openvpn) || return; packages+=("$package") ;;
      dnf) packages+=(NetworkManager-openvpn) ;;
    esac
  fi
  if ! command -v docker >/dev/null; then
    case "$VPNKIT_PACKAGE_MANAGER" in apt) packages+=(docker.io) ;; pacman) packages+=(docker) ;; dnf) packages+=(moby-engine) ;; esac
  elif docker --version 2>/dev/null | grep -qi podman; then
    vpnkit_dependency_error 'Найден адаптер Podman вместо Docker. Установщик не заменяет существующий контейнерный движок.'; return 10
  fi
  if ! docker compose version >/dev/null 2>&1; then
    compose_package=$(vpnkit_choose_package docker-compose-plugin docker-compose-v2 docker-compose) || return
    packages+=("$compose_package")
  fi
  if ! docker buildx version >/dev/null 2>&1; then
    buildx_package=$(vpnkit_choose_package docker-buildx-plugin docker-buildx) || return
    packages+=("$buildx_package")
  fi
  if (( ${#packages[@]} )); then
    printf 'Устанавливаем зависимости: %s\n' "${packages[*]}"
    case "$VPNKIT_PACKAGE_MANAGER" in
      apt) vpnkit_as_root apt-get update && vpnkit_as_root apt-get install -y "${packages[@]}" ;;
      pacman) vpnkit_as_root pacman -S --needed "${packages[@]}" ;;
      dnf) vpnkit_as_root dnf install -y "${packages[@]}" ;;
    esac || return
  fi
  vpnkit_go_ready || { vpnkit_dependency_error 'В репозиториях системы слишком старый Go: нужен Go 1.22+. Используй Ubuntu 24.04+, Debian 13+ или актуальную Fedora/Arch.'; return 10; }
  vpnkit_bun_ready || vpnkit_install_bun || return
  vpnkit_bun_ready || { vpnkit_dependency_error 'Не удалось установить Bun 1.3+.'; return 10; }
  docker compose version >/dev/null || return
  docker buildx version >/dev/null || return
  if ! docker info >/dev/null 2>&1; then
    local endpoint
    endpoint=${DOCKER_HOST:-$(docker context inspect --format '{{.Endpoints.docker.Host}}' 2>/dev/null)}
    [[ "$endpoint" == unix:///var/run/docker.sock || "$endpoint" == unix:///run/docker.sock ]] || { vpnkit_dependency_error 'Выбранный Docker-контекст недоступен. Проверь его подключение.'; return 10; }
    vpnkit_as_root systemctl enable --now docker || return
    if ! docker info >/dev/null 2>&1; then
      vpnkit_as_root usermod -aG docker "$(id -un)" || return
    fi
  fi
  if ! systemctl is-active --quiet NetworkManager; then
    vpnkit_as_root systemctl enable --now NetworkManager || return
  fi
}
# Activate newly added Docker membership without requiring desktop logout.
# Re-exec remains the desktop user, never root; quote every argument for sg.
vpnkit_docker_group_session() {
  local current user quoted argument escaped
  current=" $(id -nG) "; user=$(id -un)
  if [[ "$current" != *' docker '* && " $(id -nG "$user") " == *' docker '* ]]; then
    [[ ${VPNKIT_DOCKER_GROUP_SESSION:-0} != 1 ]] || { vpnkit_dependency_error 'Не удалось активировать группу docker.'; return 10; }
    quoted=''
    for argument in "$@"; do
      escaped=${argument//\'/\'\\\'\'}
      quoted+=" '$escaped'"
    done
    export VPNKIT_DOCKER_GROUP_SESSION=1
    exec sg docker -c "$quoted"
  fi
}
