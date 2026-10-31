#!/bin/sh
# fetchit installer — Linux, macOS, WSL, Git Bash
set -eu

REPO="${FETCHIT_REPO:-https://raw.githubusercontent.com/YOU/fetchit/main}"
PREFIX="${FETCHIT_PREFIX:-$HOME/.local/bin}"

say() { printf '\033[38;5;39m>\033[0m %s\n' "$*"; }
die() { printf '\033[38;5;203m✗\033[0m %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Linux*|Darwin*|MINGW*|MSYS*|CYGWIN*) : ;;
  *) die "unsupported: $(uname -s). WSL or Git Bash required on Windows." ;;
esac

command -v bash >/dev/null 2>&1 || die "bash not found"
command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1 || die "need curl or wget"

mkdir -p "$PREFIX"
say "installing to $PREFIX/fetchit"

if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$REPO/fetchit.sh" -o "$PREFIX/fetchit"
else
  wget -q "$REPO/fetchit.sh" -O "$PREFIX/fetchit"
fi

chmod +x "$PREFIX/fetchit"

# PATH hint
case ":$PATH:" in
  *":$PREFIX:"*) : ;;
  *)
    say "adding $PREFIX to PATH in ~/.bashrc / ~/.zshrc"
    for rc in "$HOME/.bashrc" "$HOME/.zshrc"; do
      [[ -f "$rc" ]] && ! grep -qF "$PREFIX" "$rc" && \
        printf '\nexport PATH="%s:$PATH"\n' "$PREFIX" >> "$rc"
    done
    say "restart your shell or run: export PATH=\"$PREFIX:\$PATH\""
    ;;
esac

say "done. run: fetchit --help"