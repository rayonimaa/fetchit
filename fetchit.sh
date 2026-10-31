#!/bin/sh
[ -n "${_CLEAN:-}" ] || { _T=$(mktemp "${TMPDIR:-/tmp}/fetchit.XXXXXX") || exit 1; tr -d '\r' < "$0" > "$_T" || exit 1; _CLEAN=1 _T="$_T" exec bash "$_T" "$@"; }; : # crlf-safe
# =============================================================================
# fetchit — codex-style YouTube downloader (yt-dlp + ffmpeg auto-bootstrap)
# v1.0 — release
# =============================================================================
set -uo pipefail
shopt -s lastpipe 2>/dev/null || true

APP="fetchit"
VERSION="1.0"
OUTPUT_DIR="$HOME/Downloads"
FORMAT="mp4"
QUALITY="best"
DRY_RUN=0
VERBOSE=0
DEBUG=0
ASSUME_YES=0
URLS=()
RAW_ARGS=("$@")
START=$SECONDS
SAVED=()
FAILED=0
BIN_DIR="${XDG_DATA_HOME:-$HOME/.local/share}/fetchit/bin"
FORCE_BOOTSTRAP=0
DO_CLEAN=0
INTERACTIVE=0
IS_PLAYLIST=0
PLAYLIST_COUNT=0
PLAYLIST_TITLE=""

VIDEO_FORMATS="mp4 mkv webm mov"
AUDIO_FORMATS="mp3 m4a opus flac wav aac"
LOSSLESS_FORMATS="flac wav"

case "${LC_ALL:-${LC_CTYPE:-${LANG:-}}}" in
  *[Uu][Tt][Ff]*) : ;;
  *) export LC_ALL=C.UTF-8 2>/dev/null || true ;;
esac

cleanup() {
  (( TTY )) && tput cnorm 2>/dev/null
  printf '\r\033[K' 2>/dev/null
  [[ -n "${_T:-}" && -f "${_T:-}" ]] && rm -f "$_T" 2>/dev/null
}
trap 'cleanup; exit 130' INT TERM
[[ -n "${_T:-}" ]] && trap cleanup EXIT

if [[ -t 1 && -z "${NO_COLOR:-}" && "${TERM:-dumb}" != "dumb" ]]; then
  TTY=1
  R=$'\e[0m'; D=$'\e[2m'; B=$'\e[1m'
  A=$'\e[38;5;39m'
  G=$'\e[38;5;42m'; Y=$'\e[38;5;214m'; E=$'\e[38;5;203m'
  L=$'\e[38;5;240m'; T=$'\e[38;5;252m'; M=$'\e[38;5;245m'
else
  TTY=0; R=""; D=""; B=""; A=""; G=""; Y=""; E=""; L=""; T=""; M=""
fi

SPIN=(⠋ ⠙ ⠹ ⠸ ⠼ ⠴ ⠦ ⠧ ⠇ ⠏)
SI=0

# ─── path helpers ──────────────────────────────────────────────────────────
to_win() {
  local p="$1" r
  command -v cygpath >/dev/null 2>&1 || { printf '%s' "$p"; return; }
  r=$(cygpath -m "$p" 2>/dev/null) && { printf '%s' "$r"; return; }
  printf '%s' "$p"
}
to_unix() {
  local p="$1" r
  command -v cygpath >/dev/null 2>&1 || { printf '%s' "$p"; return; }
  r=$(cygpath -u "$p" 2>/dev/null) && { printf '%s' "$r"; return; }
  printf '%s' "$p"
}

# ─── width math ────────────────────────────────────────────────────────────
_vlen_py='
import sys, unicodedata
s=sys.stdin.read().rstrip("\n"); w=0
for c in s:
    if unicodedata.combining(c): continue
    w += 2 if unicodedata.east_asian_width(c) in ("W","F") else 1
print(w)
'
_trunc_py='
import sys, unicodedata
max_w=int(sys.argv[1]); s=sys.stdin.read()
out, w = [], 0
for c in s:
    cw = 0 if unicodedata.combining(c) else (2 if unicodedata.east_asian_width(c) in ("W","F") else 1)
    if w + cw > max_w - 1: out.append("…"); break
    out.append(c); w += cw
sys.stdout.write("".join(out))
'
command -v python3 >/dev/null 2>&1 && HAVE_PY=1 || HAVE_PY=0

strip_ansi_bash() {
  local s="$1"
  while [[ "$s" == *$'\e'* ]]; do
    local changed=0
    while [[ "$s" =~ $'\e'\[[0-9\;]*[A-Za-z] ]]; do
      s="${s//${BASH_REMATCH[0]}/}"
      changed=1
    done
    (( changed )) || break
  done
  printf '%s' "$s"
}

vlen() {
  local s="${1-}"
  if [[ "$s" != *$'\e'* && "$s" != *[![:ascii:]]* ]]; then
    printf '%s' "${#s}"
    return
  fi
  [[ "$s" == *$'\e'* ]] && s=$(strip_ansi_bash "$s")
  if [[ "$s" != *[![:ascii:]]* ]]; then
    printf '%s' "${#s}"
    return
  fi
  local n
  if (( HAVE_PY )); then n=$(printf '%s' "$s" | python3 -c "$_vlen_py" 2>/dev/null); fi
  [[ -z "${n:-}" || ! "${n:-}" =~ ^[0-9]+$ ]] && n="${#s}"
  printf '%s' "$n"
}

trunc() {
  local s="${1-}" max="${2:-999}"
  if [[ "$s" != *[![:ascii:]]* ]]; then
    if (( ${#s} <= max )); then printf '%s' "$s"
    else printf '%s…' "${s:0:max-1}"; fi
    return
  fi
  local out
  if (( HAVE_PY )); then
    out=$(printf '%s' "$s" | python3 -c "$_trunc_py" "$max" 2>/dev/null)
    [[ -n "$out" ]] && { printf '%s' "$out"; return; }
  fi
  if (( ${#s} > max )); then printf '%s…' "${s:0:max-1}"; else printf '%s' "$s"; fi
}

if (( BASH_VERSINFO[0] >= 4 )); then
  declare -A _REP_CACHE=()
  rep() {
    local n="${1:-0}" c="${2:- }"
    n="${n//[!0-9]/}"; [[ -z "$n" ]] && n=0
    (( n < 0 )) && n=0
    [[ -z "$c" ]] && c=' '
    local key="${n}:${c}"
    if [[ -n "${_REP_CACHE[$key]:-}" ]]; then
      printf '%s' "${_REP_CACHE[$key]}"
      return
    fi
    local s=""
    while (( n-- > 0 )); do s+="$c"; done
    _REP_CACHE[$key]="$s"
    printf '%s' "$s"
  }
else
  rep() {
    local n="${1:-0}" c="${2:- }"
    n="${n//[!0-9]/}"; [[ -z "$n" ]] && n=0
    (( n < 0 )) && n=0
    [[ -z "$c" ]] && c=' '
    local s=""
    while (( n-- > 0 )); do s+="$c"; done
    printf '%s' "$s"
  }
fi

getsize() {
  local f="$1" s
  s=$(stat -c%s "$f" 2>/dev/null) || s=$(stat -f%z "$f" 2>/dev/null) || s=0
  printf '%s' "${s:-0}"
}
human() {
  local b=${1:-0}
  if   (( b >= 1073741824 )); then awk -v s="$b" 'BEGIN{printf "%.1f GiB",s/1073741824}'
  elif (( b >= 1048576 ));    then awk -v s="$b" 'BEGIN{printf "%.1f MiB",s/1048576}'
  elif (( b >= 1024 ));       then awk -v s="$b" 'BEGIN{printf "%.1f KiB",s/1024}'
  else                             printf '%d B' "$b"
  fi
}

# ─── layout ────────────────────────────────────────────────────────────────
BOX_W=54
if [[ -t 1 ]]; then
  _c=$(tput cols 2>/dev/null || echo 80)
  [[ "$_c" =~ ^[0-9]+$ ]] || _c=80
  if   (( _c < 40 )); then BOX_W=32
  elif (( _c < 58 )); then BOX_W=$(( _c - 4 ))
  fi
fi

box_top() { printf '%s╭%s╮%s\n' "$L" "$(rep $((BOX_W-2)) '─')" "$R"; }
box_bot() { printf '%s╰%s╯%s\n' "$L" "$(rep $((BOX_W-2)) '─')" "$R"; }
box_row() {
  local text="${1-}" pad
  pad=$(( BOX_W - 4 - $(vlen "$text") )); (( pad < 0 )) && pad=0
  printf '%s│%s %s%s %s│%s\n' "$L" "$R" "$text" "$(rep "$pad" ' ')" "$L" "$R"
}

header() {
  local dir_disp="${OUTPUT_DIR/#$HOME/~}"
  dir_disp=$(trunc "$dir_disp" $((BOX_W - 14)))
  local l1="${B}${A}>_ ${APP}${R} ${M}(v${VERSION})${R}"
  local l3="engine:    yt-dlp + ffmpeg"
  local l4="directory: ${dir_disp}"
  local pad

  printf '\n'
  box_top
  box_row "$l1"
  box_row ""
  pad=$(( BOX_W - 4 - $(vlen "$l3") )); (( pad < 0 )) && pad=0
  printf '%s│%s %s%s%s %s│%s\n' "$L" "$R" "$M" "$l3" "$R" "$(rep "$pad" ' ')" "$L" "$R"
  pad=$(( BOX_W - 4 - $(vlen "$l4") )); (( pad < 0 )) && pad=0
  printf '%s│%s %s%s%s %s│%s\n' "$L" "$R" "$M" "$l4" "$R" "$(rep "$pad" ' ')" "$L" "$R"
  box_bot
}

echo_input() { printf '\n%s›%s %s%s %s%s%s\n\n' "$A" "$R" "$B" "$APP" "$T" "${RAW_ARGS[*]:-}" "$R"; }
phase()   { printf '\n%s•%s %s%s%s\n' "$A" "$R" "$B" "$1" "$R"; }
sub()     { printf '  %s│%s %s%s%s\n' "$L" "$R" "$M" "$1" "$R"; }
sub_kv()  { printf '  %s│%s %s%-9s%s %s%s%s\n' "$L" "$R" "$M" "$1" "$R" "$T" "$2" "$R"; }
tool()    { printf '  %s↳%s %s%s%s\n' "$L" "$R" "$D" "$1" "$R"; }
ok()      { printf '  %s✓%s %s%s%s\n' "$G" "$R" "$T" "$1" "$R"; }
err()     { printf '  %s✗%s %s%s%s\n' "$E" "$R" "$T" "$1" "$R"; }
warn()    { printf '  %s△%s %s%s%s\n' "$Y" "$R" "$T" "$1" "$R"; }

footer() {
  local elapsed=$(( SECONDS - START )) total=0 f h n
  for f in "${SAVED[@]}"; do total=$(( total + $(getsize "$f") )); done
  h=$(human "$total"); n=${#SAVED[@]}
  printf '\n  %s%s%s\n' "$L" "$(rep "$BOX_W" '─')" "$R"
  printf '  %s%d file%s · %s · %ds%s' "$M" "$n" "$([[ $n -eq 1 ]] && echo '' || echo 's')" "$h" "$elapsed" "$R"
  (( FAILED > 0 )) && printf ' · %s%d failed%s' "$E" "$FAILED" "$R"
  printf '\n\n'
}

die() {
  (( TTY )) && tput cnorm 2>/dev/null
  phase "Error"
  printf '  %s│%s %s%s%s\n\n' "$L" "$R" "$E" "$1" "$R" >&2
  exit 1
}

# ─── single-line progress ──────────────────────────────────────────────────
progress_line() {
  local pct="${1-}" spd="${2-}" eta="${3-}" status="${4-}"
  pct="${pct//[[:space:]]/}"; spd="${spd//[[:space:]]/}"; eta="${eta//[[:space:]]/}"
  [[ -z "$pct" || "$pct" == "NA%" ]] && pct="--%"
  [[ -z "$spd" || "$spd" == "NA"  ]] && spd="--"
  [[ -z "$eta" || "$eta" == "NA"  ]] && eta="--"

  local n="${pct%%.*}"; n="${n%\%}"
  [[ "$n" =~ ^[0-9]+$ ]] || n=0
  (( n > 100 )) && n=100; (( n < 0 )) && n=0

  local bar_w=20
  local filled=$(( n * bar_w / 100 ))
  local empty=$(( bar_w - filled ))
  local sp="${SPIN[SI]}"; SI=$(( (SI+1) % 10 ))

  (( TTY )) && printf '\r\033[K'
  printf '  %s%s%s  %s%s%s%s%s  %s%3d%%%s  %s·%s %s%s%s  %s·%s %seta %s%s  %s│%s %s%s%s' \
    "$A" "$sp" "$R" \
    "$A" "$(rep $filled '█')" "$L" "$(rep $empty '░')" "$R" \
    "$T" "$n" "$R" \
    "$L" "$R" "$M" "$spd" "$R" \
    "$L" "$R" "$M" "$eta" "$R" \
    "$L" "$R" "$M" "$status" "$R"
}

progress_log() {
  (( TTY )) && printf '\r\033[K'
  printf '%s\n' "$1"
}

progress_finish() { (( TTY )) && printf '\r\033[K'; }

# ─── interactive menu ──────────────────────────────────────────────────────
select_option() {
  local prompt="$1"; shift
  local -a options=("$@")
  local selected=0 num_opts=${#options[@]} key key2 i

  (( TTY )) && tput civis 2>/dev/null
  (( TTY )) && printf '\033[s'

  while true; do
    (( TTY )) && printf '\033[u\033[J'
    printf '  %s│%s %s%s%s\n' "$L" "$R" "$B$T" "$prompt" "$R"
    for i in "${!options[@]}"; do
      if (( i == selected )); then
        printf '  %s│%s   %s▸ %s%s%s\n' "$L" "$R" "$A" "${options[$i]}" "$R" "$B"
      else
        printf '  %s│%s     %s%s%s\n' "$L" "$R" "$M" "${options[$i]}" "$R"
      fi
    done

    IFS= read -rsn1 key
    case "$key" in
      $'\x1b')
        IFS= read -rsn2 -t 0.05 key2 || key2=""
        case "$key2" in
          '[A') (( selected = (selected - 1 + num_opts) % num_opts )) ;;
          '[B') (( selected = (selected + 1) % num_opts )) ;;
        esac ;;
      ""|$'\n'|$'\r') break ;;
    esac
  done

  (( TTY )) && tput cnorm 2>/dev/null
  (( TTY )) && printf '\033[u\033[J'
  printf '  %s│%s %s✓%s %s%s%s\n' "$L" "$R" "$G" "$R" "$T" "${options[$selected]}" "$R"

  return "$selected"
}

ask_input() {
  local __prompt="$1" __default="$2" __var="$3" __value

  (( TTY )) && printf '\033[s'
  printf '  %s│%s %s%s%s ' "$L" "$R" "$M" "$__prompt" "$R"
  IFS= read -r __value
  [[ -z "$__value" && -n "$__default" ]] && __value="$__default"

  (( TTY )) && printf '\033[u\033[J'
  printf '  %s│%s %s✓%s %s%s%s\n' "$L" "$R" "$G" "$R" "$T" "$__value" "$R"

  printf -v "$__var" '%s' "$__value"
}

# ─── platform + fetchers ───────────────────────────────────────────────────
detect_platform() {
  OS=$(uname -s | tr '[:upper:]' '[:lower:]'); ARCH=$(uname -m)
  case "$OS" in
    linux*)  OS="linux" ;;
    darwin*) OS="macos" ;;
    mingw*|msys*|cygwin*) OS="windows" ;;
    *) die "unsupported OS: $OS" ;;
  esac
  case "$ARCH" in
    x86_64|amd64)  ARCH="x86_64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    armv7l|armhf)  ARCH="armv7l" ;;
    *) die "unsupported architecture: $ARCH" ;;
  esac
}

fetch_url() {
  local url="$1" dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL --retry 2 --retry-delay 1 --connect-timeout 10 --max-time 240 -o "$dest" "$url"
  elif command -v wget >/dev/null 2>&1; then
    wget -q --tries=2 --timeout=30 -O "$dest" "$url"
  else
    die "curl or wget required for bootstrap"
  fi
}

bootstrap_ytdlp() {
  local dest url tmp inner
  case "$OS" in
    linux)
      case "$ARCH" in
        x86_64) url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux" ;;
        arm64)  url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux_aarch64" ;;
        armv7l) url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux_armv7l.zip" ;;
      esac
      dest="$BIN_DIR/yt-dlp" ;;
    macos)
      url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_macos"
      dest="$BIN_DIR/yt-dlp" ;;
    windows)
      url="https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
      dest="$BIN_DIR/yt-dlp.exe" ;;
  esac
  if [[ "$url" == *.zip ]]; then
    tmp=$(mktemp -d)
    fetch_url "$url" "$tmp/a.zip" || { rm -rf "$tmp"; return 1; }
    ( cd "$tmp" && unzip -q a.zip ) || { rm -rf "$tmp"; return 1; }
    inner=$(find "$tmp" -maxdepth 1 -type f -name 'yt-dlp*' ! -name '*.zip' | head -n1)
    [[ -z "$inner" ]] && { rm -rf "$tmp"; return 1; }
    mv "$inner" "$dest"; rm -rf "$tmp"
  else
    fetch_url "$url" "$dest" || return 1
  fi
  chmod +x "$dest" 2>/dev/null
  return 0
}

bootstrap_ffmpeg() {
  local ff_url tarball=0 tmp ff ffp variant
  tmp=$(mktemp -d)
  variant="${FETCHIT_GPL:+gpl}"; variant="${variant:-lgpl}"

  case "$OS" in
    linux)
      case "$ARCH" in
        x86_64) ff_url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-linux64-${variant}.tar.xz" ;;
        arm64)  ff_url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-linuxarm64-${variant}.tar.xz" ;;
        *) rm -rf "$tmp"; return 1 ;;
      esac
      tarball=1 ;;
    macos)
      case "$ARCH" in
        x86_64) ff_url="https://evermeet.cx/ffmpeg/getrelease/zip" ;;
        arm64)  ff_url="https://www.osxexperts.net/ffmpeg7arm.zip" ;;
      esac ;;
    windows) ff_url="https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-master-latest-win64-${variant}.zip" ;;
  esac

  if (( tarball )); then
    fetch_url "$ff_url" "$tmp/ff.tar.xz" || { rm -rf "$tmp"; return 1; }
    tar -xf "$tmp/ff.tar.xz" -C "$tmp" || { rm -rf "$tmp"; return 1; }
    ff=$(find "$tmp" -type f -name 'ffmpeg' -path '*/bin/*' | head -n1)
    [[ -z "$ff" ]] && ff=$(find "$tmp" -type f -name 'ffmpeg' | head -n1)
    [[ -z "$ff" ]] && { rm -rf "$tmp"; return 1; }
    ffp=$(find "$tmp" -type f -name 'ffprobe' -path '*/bin/*' | head -n1)
    [[ -z "$ffp" ]] && ffp=$(find "$tmp" -type f -name 'ffprobe' | head -n1)
    cp "$ff" "$BIN_DIR/ffmpeg"
    [[ -n "$ffp" ]] && cp "$ffp" "$BIN_DIR/ffprobe"
  else
    fetch_url "$ff_url" "$tmp/ff.zip" || { rm -rf "$tmp"; return 1; }
    ( cd "$tmp" && unzip -q ff.zip ) || { rm -rf "$tmp"; return 1; }
    ff=$(find "$tmp" -type f -name 'ffmpeg.exe' -o -type f -name 'ffmpeg' | head -n1)
    ffp=$(find "$tmp" -type f -name 'ffprobe.exe' -o -type f -name 'ffprobe' | head -n1)
    [[ -z "$ff" ]] && { rm -rf "$tmp"; return 1; }
    if [[ "$OS" == "windows" ]]; then
      cp "$ff" "$BIN_DIR/ffmpeg.exe"
      [[ -n "$ffp" ]] && cp "$ffp" "$BIN_DIR/ffprobe.exe"
    else
      cp "$ff" "$BIN_DIR/ffmpeg"
      [[ -n "$ffp" ]] && cp "$ffp" "$BIN_DIR/ffprobe"
    fi
  fi

  chmod +x "$BIN_DIR"/ffmpeg* "$BIN_DIR"/ffprobe* 2>/dev/null
  rm -rf "$tmp"
  return 0
}

bootstrap_all() {
  local need_yt=$1 need_ff=$2
  local pids=() labels=()

  phase "Bootstrapping dependencies"
  sub "platform  ${OS}/${ARCH}"
  sub "bin dir   ${BIN_DIR}"

  if (( need_yt )); then
    ( bootstrap_ytdlp ) & pids+=($!); labels+=("yt-dlp")
  fi
  if (( need_ff )); then
    ( bootstrap_ffmpeg ) & pids+=($!); labels+=("ffmpeg")
  fi

  local si=0 alive
  if (( TTY && ${#pids[@]} > 0 )); then
    while :; do
      alive=0
      for pid in "${pids[@]}"; do kill -0 "$pid" 2>/dev/null && alive=1; done
      (( alive )) || break
      printf '\r  %s│%s %s%s fetching…%s' "$L" "$R" "$A" "${SPIN[si]}" "$R"
      si=$(( (si+1) % 10 ))
      sleep 0.1
    done
    printf '\r\033[K'
  fi

  local i=0
  for pid in "${pids[@]}"; do
    if wait "$pid"; then
      (( need_yt && i == 0 )) && ok "yt-dlp ready"
      (( need_ff && (i == 1 || need_yt == 0) )) && ok "ffmpeg ready"
    else
      die "${labels[$i]} bootstrap failed"
    fi
    i=$((i+1))
  done

  rm -f "$BIN_DIR/.encoders_ok"
}

ensure_tools() {
  mkdir -p "$BIN_DIR" || die "cannot create $BIN_DIR"
  export PATH="$BIN_DIR:$PATH"

  local need_yt=0 need_ff=0
  (( FORCE_BOOTSTRAP )) && { need_yt=1; need_ff=1; }
  command -v yt-dlp >/dev/null 2>&1 || need_yt=1
  command -v ffmpeg >/dev/null 2>&1 || need_ff=1

  if (( need_yt || need_ff )); then
    detect_platform
    bootstrap_all "$need_yt" "$need_ff"
  fi

  command -v yt-dlp >/dev/null 2>&1 || die "yt-dlp not on PATH"
  command -v ffmpeg  >/dev/null 2>&1 || die "ffmpeg not on PATH"

  local ff_bin enc_cache
  ff_bin=$(command -v ffmpeg)
  enc_cache="$BIN_DIR/.encoders_ok"
  if [[ ! -f "$enc_cache" ]] || [[ "$ff_bin" -nt "$enc_cache" ]]; then
    if ! ffmpeg -hide_banner -encoders 2>/dev/null | grep -qiE 'aac|libmp3lame'; then
      die "ffmpeg appears broken (no aac/libmp3lame encoders)"
    fi
    touch "$enc_cache"
  fi

  (( VERBOSE )) && {
    phase "Tools ready"
    sub "yt-dlp  $(yt-dlp --version 2>/dev/null | head -n1 || echo '?')"
    sub "ffmpeg  $(ffmpeg -hide_banner -version 2>/dev/null | head -n1 | awk '{print $3}' || echo '?')"
  }
}

# ─── playlist detection ────────────────────────────────────────────────────
detect_playlist() {
  local url="$1"
  local count
  count=$(yt-dlp --flat-playlist --skip-download --print "%(playlist_count)s" "$url" 2>/dev/null | head -1)
  if [[ "$count" =~ ^[0-9]+$ ]] && (( count > 1 )); then
    PLAYLIST_COUNT="$count"
    return 0
  fi
  PLAYLIST_COUNT=0
  return 1
}

# ─── args ──────────────────────────────────────────────────────────────────
while (( $# )); do
  case "$1" in
    -f|--format)    FORMAT="${2-}";     shift 2 ;;
    --format=*)     FORMAT="${1#*=}";   shift ;;
    -q|--quality)   QUALITY="${2-}";    shift 2 ;;
    --quality=*)    QUALITY="${1#*=}";  shift ;;
    -o|--output)    OUTPUT_DIR="${2-}"; shift 2 ;;
    --output=*)     OUTPUT_DIR="${1#*=}"; shift ;;
    -V|--verbose)   VERBOSE=1; shift ;;
    --debug)        DEBUG=1; VERBOSE=1; shift ;;
    --bootstrap)    FORCE_BOOTSTRAP=1; shift ;;
    --clean)        DO_CLEAN=1; shift ;;
    -y|--yes)       ASSUME_YES=1; shift ;;
    -m|--mp3)       FORMAT="mp3"; shift ;;
    -v|--mp4)       FORMAT="mp4"; shift ;;
    -n|--dry-run)   DRY_RUN=1; shift ;;
    -h|--help)
      header
      phase "Usage"
      printf '  %s%s%s %s[options] <url>...%s\n\n' "$T" "$APP" "$R" "$M" "$R"
      phase "Format"
      printf '  %s-f, --format%s   %svideo: %s%s\n' "$A" "$R" "$M" "$VIDEO_FORMATS" "$R"
      printf '  %s              %saudio: %s%s\n' "" "$M" "$AUDIO_FORMATS" "$R"
      phase "Quality"
      printf '  %s-q, --quality%s  %svideo: best 8k 4k 2k 1440 1080 720 480 360%s\n' "$A" "$R" "$M" "$R"
      printf '  %s              %saudio: best 320 256 192 128 (kbps)%s\n' "" "$M" "$R"
      phase "Options"
      printf '  %s-o, --output%s   %sinitial save directory (prompt at end can override)%s\n' "$A" "$R" "$M" "$R"
      printf '  %s-y, --yes%s      %sskip end-of-run prompts%s\n' "$A" "$R" "$M" "$R"
      printf '  %s-V, --verbose%s  %sfull yt-dlp log + tool versions%s\n' "$A" "$R" "$M" "$R"
      printf '  %s    --debug%s    %sextra diagnostics%s\n' "$A" "$R" "$M" "$R"
      printf '  %s    --bootstrap%s %sforce refresh yt-dlp + ffmpeg%s\n' "$A" "$R" "$M" "$R"
      printf '  %s    --clean%s    %sremove cached binaries%s\n' "$A" "$R" "$M" "$R"
      printf '  %s-n, --dry-run%s  %sprint resolved command%s\n' "$A" "$R" "$M" "$R"
      printf '\n'
      phase "Playlist"
      printf '  %sAuto-detected from URL — no flag needed.%s\n' "$M" "$R"
      printf '  %sPlaylists save as: <playlist>/<index> - <title>.<ext>%s\n' "$M" "$R"
      printf '  %sSingle video saves as: <title>.<ext>%s\n' "$M" "$R"
      printf '  %sMetadata is embedded in the file, not the filename.%s\n' "$M" "$R"
      printf '\n'
      exit 0 ;;
    --version)      printf '%s %s\n' "$APP" "$VERSION"; exit 0 ;;
    -*)             die "unknown option: $1" ;;
    *)              URLS+=("$1"); shift ;;
  esac
done

if (( DO_CLEAN )); then
  if [[ -d "$BIN_DIR" ]]; then
    du_size=$(du -sh "$BIN_DIR" 2>/dev/null | cut -f1)
    rm -rf "$BIN_DIR"
    printf '\n  %s✓%s removed %s (%s freed)\n\n' "$G" "$R" "$BIN_DIR" "${du_size:-?}"
  else
    printf '\n  %s△%s nothing to clean\n\n' "$Y" "$R"
  fi
  exit 0
fi

if (( ${#URLS[@]} == 0 )); then
  (( ! TTY )) && die "no URL given and stdin is not a terminal"
  INTERACTIVE=1
  header

  select_option "Type" "video" "audio"
  case $? in 0) m_type="video" ;; 1) m_type="audio" ;; esac

  if [[ "$m_type" == "video" ]]; then
    select_option "Format" "mp4" "mkv" "webm"
    case $? in 0) FORMAT="mp4" ;; 1) FORMAT="mkv" ;; 2) FORMAT="webm" ;; esac
    select_option "Quality" "best" "8k" "4k" "1080" "720" "480"
    case $? in 0) QUALITY="best" ;; 1) QUALITY="8k" ;; 2) QUALITY="4k" ;; 3) QUALITY="1080" ;; 4) QUALITY="720" ;; 5) QUALITY="480" ;; esac
  else
    select_option "Format" "mp3" "m4a" "flac"
    case $? in 0) FORMAT="mp3" ;; 1) FORMAT="m4a" ;; 2) FORMAT="flac" ;; esac
    select_option "Quality" "best" "320" "256" "128"
    case $? in 0) QUALITY="best" ;; 1) QUALITY="320" ;; 2) QUALITY="256" ;; 3) QUALITY="128" ;; esac
  fi

  ask_input "Directory" "$OUTPUT_DIR" OUTPUT_DIR
  url_in=""
  ask_input "URL" "" url_in
  [[ -z "$url_in" ]] && die "URL cannot be empty"
  URLS=("$url_in")
else
  header
  echo_input
fi

FORMAT="${FORMAT,,}"
if   [[ " $VIDEO_FORMATS " == *" $FORMAT "* ]]; then MEDIA_TYPE="video"
elif [[ " $AUDIO_FORMATS " == *" $FORMAT "* ]]; then MEDIA_TYPE="audio"
else die "unsupported format '$FORMAT'"
fi

Q="${QUALITY,,}"; Q="${Q%p}"

if [[ "$MEDIA_TYPE" == "video" ]]; then
  case "$Q" in
    best)    HFILTER="" ;;
    8k)      HFILTER="[height<=4320]" ;;
    4k)      HFILTER="[height<=2160]" ;;
    2k|1440) HFILTER="[height<=1440]" ;;
    *)
      if [[ "$Q" =~ ^[0-9]+$ ]]; then HFILTER="[height<=${Q}]"
      else die "invalid video quality '$QUALITY'"; fi ;;
  esac
  case "$FORMAT" in
    mp4|mov) VCODEC="[vcodec^=avc]";  ACODEC="[acodec^=mp4a]" ;;
    webm)    VCODEC="[vcodec^=vp9]";  ACODEC="[acodec^=opus]" ;;
    *)       VCODEC="";               ACODEC="" ;;
  esac
  if [[ -z "$HFILTER" ]]; then
    FMT_STR="bestvideo${VCODEC}+bestaudio${ACODEC}/bestvideo+bestaudio/best"
    QDESC="best"
  else
    FMT_STR="bestvideo${HFILTER}${VCODEC}+bestaudio${ACODEC}/bestvideo${HFILTER}+bestaudio/best${HFILTER}/best"
    QDESC="≤ ${Q}p"
  fi
else
  if   [[ " $LOSSLESS_FORMATS " == *" $FORMAT "* ]]; then QDESC="lossless"
  elif [[ "$Q" == "best" ]];     then QDESC="best"
  elif [[ "$Q" =~ ^[0-9]+$ ]];   then QDESC="${Q} kbps"
  else die "invalid audio quality '$QUALITY'"; fi
fi

mkdir -p "$OUTPUT_DIR" 2>/dev/null || die "cannot create directory: $OUTPUT_DIR"

NATIVE_OUTDIR=$(to_win "$OUTPUT_DIR")
NATIVE_BIN=$(to_win "$BIN_DIR")

# ─── build yt-dlp argv ─────────────────────────────────────────────────────
DL_ARGS=()
build_args() {
  local url="$1" tmpfile="$2"
  local native_tmp
  native_tmp=$(to_win "$tmpfile")

  local out_tpl
  if (( IS_PLAYLIST )); then
    out_tpl="${NATIVE_OUTDIR}/%(playlist_title|Playlist)s/%(playlist_index)s - %(title)s.%(ext)s"
  else
    out_tpl="${NATIVE_OUTDIR}/%(title)s.%(ext)s"
  fi

  DL_ARGS=(
    --no-colors --newline --no-mtime
    --ffmpeg-location "$NATIVE_BIN"
    --progress-template "download:PROGRESS:%(progress._percent_str)s|%(progress._speed_str)s|%(progress._eta_str)s"
    --print-to-file "after_move:filepath" "$native_tmp"
    --output "$out_tpl"
    --convert-thumbnails jpg
  )
  (( TTY )) || DL_ARGS+=(--no-progress)

  if (( IS_PLAYLIST )); then
    DL_ARGS+=(--yes-playlist)
  else
    DL_ARGS+=(--no-playlist)
  fi

  if [[ "$MEDIA_TYPE" == "video" ]]; then
    DL_ARGS+=(--format "$FMT_STR" --merge-output-format "$FORMAT" --remux-video "$FORMAT")
    DL_ARGS+=(--embed-metadata --embed-thumbnail)
  else
    DL_ARGS+=(--extract-audio --audio-format "$FORMAT")
    if   [[ " $LOSSLESS_FORMATS " == *" $FORMAT "* ]]; then :
    elif [[ "$Q" == "best" ]]; then DL_ARGS+=(--audio-quality 0)
    else                             DL_ARGS+=(--audio-quality "${Q}K")
    fi
    DL_ARGS+=(--embed-metadata)
    [[ "$FORMAT" != "wav" ]] && DL_ARGS+=(--embed-thumbnail)
  fi
  DL_ARGS+=("$url")
}

# ─── resolve ───────────────────────────────────────────────────────────────
resolve() {
  local url="$1" out errf rc title dur chan errmsg

  phase "Resolving"
  (( TTY )) && printf '  %s│%s %sresolving…%s' "$L" "$R" "$M" "$R"

  errf=$(mktemp)
  out=$(yt-dlp --no-warnings --skip-download \
    --ffmpeg-location "$NATIVE_BIN" \
    --print "%(title)s" \
    --print "%(duration_string)s" \
    --print "%(uploader,channel|unknown)s" \
    --print "%(playlist_title)s" \
    "$url" 2>"$errf")
  rc=$?

  (( TTY )) && printf '\r\033[K'

  if (( rc != 0 )); then
    errmsg=$(grep -iE 'error|unavailable|private|sign in' "$errf" | head -n1)
    [[ -z "$errmsg" ]] && errmsg=$(head -n1 "$errf")
    warn "resolve failed (exit $rc): ${errmsg:-unknown}"
    (( DEBUG )) && { printf '───── stderr ─────\n'; cat "$errf"; printf '──────────────────\n'; }
    rm -f "$errf"
    return 1
  fi
  rm -f "$errf"

  title=$(printf '%s\n' "$out" | sed -n 1p | tr -d '\r')
  dur=$(printf '%s\n' "$out" | sed -n 2p | tr -d '\r')
  chan=$(printf '%s\n' "$out" | sed -n 3p | tr -d '\r')
  PLAYLIST_TITLE=$(printf '%s\n' "$out" | sed -n 4p | tr -d '\r')

  [[ -z "$title" ]] && { warn "video unavailable or private"; return 1; }

  sub_kv "title" "$(trunc "$title" $((BOX_W - 14)))"
  [[ -n "$dur" && "$dur" != "NA" ]] && sub_kv "duration" "$dur"
  [[ -n "$chan" && "$chan" != "unknown" ]] && sub_kv "channel" "$(trunc "$chan" $((BOX_W - 14)))"
  return 0
}

# ─── download ──────────────────────────────────────────────────────────────
download_one() {
  local url="$1" tmpfile line rc added _ln _last
  local curr_status="fetching media…"
  local pct="--" spd="--" eta="--"

  phase "Downloading $MEDIA_TYPE → $FORMAT @ $QDESC"
  (( IS_PLAYLIST )) && sub "playlist  $(trunc "$PLAYLIST_TITLE" $((BOX_W - 14)))  (${PLAYLIST_COUNT} videos)"

  tmpfile=$(mktemp)
  build_args "$url" "$tmpfile"

  if (( VERBOSE )); then
    tool "yt-dlp ${DL_ARGS[*]}"
    printf '\n'
    yt-dlp "${DL_ARGS[@]}" 2>&1 | while IFS= read -r line; do
      case "$line" in
        PROGRESS:*) ;;
        "") ;;
        *) sub "$line" ;;
      esac
    done
    rc=${PIPESTATUS[0]}
  else
    yt-dlp "${DL_ARGS[@]}" 2>&1 | while IFS= read -r line; do
      case "$line" in
        PROGRESS:*)
          local rest="${line#PROGRESS:}"
          IFS='|' read -r pct spd eta <<< "$rest"
          progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[download] Destination:"*)
          curr_status="saving segments…"
          progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[Merger]"*)              curr_status="merging streams…";      progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[ExtractAudio]"*)        curr_status="extracting audio…";     progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[VideoConvertor]"*)      curr_status="converting format…";    progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[FixupM3u8]"*)           curr_status="fixing m3u8…";          progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[EmbedThumbnail]"*)      curr_status="embedding cover art…";  progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[Metadata]"*)            curr_status="writing metadata…";     progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[MoveFiles]"*)           curr_status="moving final file…";    progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "[ThumbnailsConvertor]"*) curr_status="converting cover…";     progress_line "$pct" "$spd" "$eta" "$curr_status" ;;
        "ERROR:"*|"[error]"*)     progress_log "  ${E}✗${R} ${line}" ;;
        "WARNING:"*|"[warning]"*) [[ "$line" != *"JavaScript runtime"* ]] && progress_log "  ${Y}△${R} ${line}" ;;
        "") ;;
        *) (( DEBUG )) && progress_log "  ${D}│ ${line}${R}" ;;
      esac
    done
    rc=${PIPESTATUS[0]}
    progress_finish
  fi

  # collect ALL file paths (playlists write one line per video)
  added=0
  if [[ -s "$tmpfile" ]]; then
    while IFS= read -r _ln; do
      _ln="${_ln%$'\r'}"
      _ln=$(to_unix "$_ln")
      [[ -n "$_ln" && -f "$_ln" ]] || continue
      SAVED+=("$_ln")
      added=$((added + 1))
    done < "$tmpfile"
  fi
  rm -f "$tmpfile"

  if (( rc != 0 )); then
    if (( added > 0 )); then
      warn "partial: exit $rc, $added file(s) saved"
      FAILED=$((FAILED + 1))
      return 0
    fi
    err "download failed (exit $rc)"
    FAILED=$((FAILED + 1))
    return 1
  fi

  if (( added == 0 )); then
    warn "no file produced (already present?)"
    return 1
  elif (( added == 1 )); then
    _last=$(( ${#SAVED[@]} - 1 ))
    ok "$(trunc "${SAVED[$_last]}" $((BOX_W + 20)))"
  else
    ok "$added files"
  fi
  return 0
}

# ─── main ──────────────────────────────────────────────────────────────────
ensure_tools

i=0
for u in "${URLS[@]}"; do
  i=$((i + 1))
  (( ${#URLS[@]} > 1 )) && phase "URL $i/${#URLS[@]}"

  IS_PLAYLIST=0
  PLAYLIST_COUNT=0
  if detect_playlist "$u"; then
    IS_PLAYLIST=1
    phase "Playlist detected"
    sub "entries  ${PLAYLIST_COUNT}"
  else
    IS_PLAYLIST=0
  fi

  if ! resolve "$u"; then
    FAILED=$((FAILED + 1)); continue
  fi

  if (( DRY_RUN )); then
    build_args "$u" /tmp/fetchit.dry
    tool "yt-dlp ${DL_ARGS[*]}"
    continue
  fi

  download_one "$u"
done

if (( DRY_RUN )); then
  printf '\n  %s(no download performed)%s\n\n' "$M" "$R"
  if (( ! ASSUME_YES )) && [[ -t 0 ]]; then
    printf '  %sPress Enter to exit…%s' "$A" "$R"
    IFS= read -r _ || true
  fi
  exit 0
fi

# ── post-download: ask where to save (blank = keep default) ────────────────
if (( ${#SAVED[@]} > 0 )) && (( ! ASSUME_YES )) && [[ -t 0 ]]; then
  printf '\n  %s│%s %sSave to [%s]:%s ' "$L" "$R" "$M" "$OUTPUT_DIR" "$R"
  IFS= read -r new_dir || new_dir=""
  new_dir="${new_dir/#\~/$HOME}"

  if [[ -z "$new_dir" || "$new_dir" == "$OUTPUT_DIR" ]]; then
    printf '  %s│%s %s✓%s %skept in place%s\n' "$L" "$R" "$G" "$R" "$M" "$R"
  else
    mkdir -p "$new_dir" 2>/dev/null
    moved=0
    failed_mv=0
    SAVED_NEW=()
    for f in "${SAVED[@]}"; do
      rel="${f#"$OUTPUT_DIR"/}"
      dest="$new_dir/$rel"
      mkdir -p "$(dirname "$dest")" 2>/dev/null
      if mv "$f" "$dest" 2>/dev/null; then
        moved=$((moved + 1))
        SAVED_NEW+=("$dest")
      else
        failed_mv=$((failed_mv + 1))
        SAVED_NEW+=("$f")
      fi
    done
    SAVED=("${SAVED_NEW[@]}")
    printf '  %s│%s %s✓%s %smoved %d file%s → %s%s\n' \
      "$L" "$R" "$G" "$R" "$M" "$moved" \
      "$([[ $moved -eq 1 ]] && echo '' || echo 's')" "$new_dir" "$R"
    (( failed_mv > 0 )) && warn "failed to move $failed_mv file(s)"
  fi
fi

footer

# ── pause so terminal doesn't auto-close ───────────────────────────────────
if (( ! ASSUME_YES )) && [[ -t 0 ]]; then
  printf '\n  %sPress Enter to exit…%s' "$A" "$R"
  IFS= read -r _ || true
fi

(( FAILED > 0 )) && exit 1
exit 0