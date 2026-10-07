#!/bin/bash
echo "$PROTON_DRIVE_CREDENTIALS_STORE|$PROTON_DRIVE_LOG_LEVEL|$PROTON_DRIVE_CACHE_DIR|$*" >> "$FAKE_ROOT/calls.log"
R="$FAKE_ROOT/drive"
if [ "$1" = "--version" ]; then echo "proton-drive 0.8.3"; echo "A newer version (0.9.0) is available"; exit 0; fi
if [ -n "$FAKE_SIGNED_OUT" ]; then echo "Error: not logged in (session expired)" >&2; exit 1; fi
if [ "$1" = "auth" ]; then
  echo "To sign in, open https://account.proton.me/device?c=1 and enter code AB12-CD34"
  exit 0
fi
shift  # filesystem
cmd=$1; shift
args=(); for a in "$@"; do case "$a" in -j|-f|-d|skip|merge) ;; *) args+=("$a");; esac; done
case "$cmd" in
  list)
    d="$R${args[0]}"; [ -d "$d" ] || { echo "Error: not found: ${args[0]}" >&2; exit 1; }
    out=""; for f in "$d"/* "$d"/.[!.]*; do [ -e "$f" ] || continue
      t=file; [ -d "$f" ] && t=folder
      out="$out{\"name\":\"$(basename "$f")\",\"type\":\"$t\",\"size\":$(stat -c %s "$f")},"
    done
    echo "[${out%,}]";;
  info)
    f="$R${args[0]}"; [ -f "$f" ] || { echo "Error: not found" >&2; exit 1; }
    s=$(stat -c %s "$f"); [ -n "$FAKE_SHORT" ] && s=$((s-1))
    echo "{\"name\":\"$(basename "$f")\",\"type\":\"file\",\"size\":$s}";;
  mkdir) mkdir -p "$R${args[0]}";;
  upload) mkdir -p "$R${args[1]}"; cp -r "${args[0]}" "$R${args[1]}/";;
  download) cp "$R${args[0]}" "${args[1]}/"; [ -n "$FAKE_CORRUPT" ] && echo x >> "${args[1]}/$(basename "${args[0]}")"; true;;
  trash) mkdir -p "$R/.trash$(dirname "${args[0]}")"; mv "$R${args[0]}" "$R/.trash${args[0]}";;
  delete) [ -e "$R/.trash${args[0]}" ] || { echo "Error: only trashed items can be deleted" >&2; exit 1; }; rm -rf "$R/.trash${args[0]}";;
  *) echo "unknown command $cmd" >&2; exit 2;;
esac

