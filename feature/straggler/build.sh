#!/usr/bin/env bash
# Provision the straggler build dependencies on an aarch64 host and compile:
#   1. check arch == aarch64 (exit otherwise)
#   2. fetch dyno/dynolog from the msmonitor 8.1.0 bundle zip
#      (skipped when 3rdparty/bin/dyno + dynolog already exist)
#   3. check python version is in {3.9, 3.10, 3.11, 3.12}
#   4. pip install the mindstudio_monitor 26.2.0 wheel (cp tag matched to python)
#      (skipped when pip already has mindstudio_monitor 26.2.0)
#   5. ensure go >= go.mod requirement (download from Aliyun mirror if missing/old)
#   6. go build
#
# Run:  bash build.sh [--insecure]
#   --insecure: add --no-check-certificate to all wget downloads (for hosts
#               whose TLS stack cannot verify the OBS / Aliyun certificates).
set -euo pipefail
cd "$(dirname "$0")"

INSECURE=0
for arg in "$@"; do
    case "$arg" in
        --insecure) INSECURE=1 ;;
        *) echo "[build] WARNING: unknown argument '$arg' ignored" >&2 ;;
    esac
done
WGET_ARGS=()
if [ "$INSECURE" = "1" ]; then
    WGET_ARGS+=(--no-check-certificate)
    echo "[build] --insecure: wget will skip certificate verification"
fi

BIN_DIR="3rdparty/bin"

# ver_ge A B: exit 0 when version A >= version B (dot-separated numbers).
ver_ge() {
    local a="$1" b="$2" ia ib
    while [ -n "$a" ] || [ -n "$b" ]; do
        ia="${a%%.*}"; [ -n "$ia" ] || ia=0
        ib="${b%%.*}"; [ -n "$ib" ] || ib=0
        if [ "$ia" -gt "$ib" ]; then return 0; fi
        if [ "$ia" -lt "$ib" ]; then return 1; fi
        a="${a#*.}"; b="${b#*.}"
    done
    return 0
}

# ---------------------------------------------------------------------------
# 1. Architecture: only aarch64 is supported
# ---------------------------------------------------------------------------
ARCH=$(uname -m)
if [ "$ARCH" != "aarch64" ]; then
    echo "[build] ERROR: unsupported architecture '$ARCH' — only aarch64 is supported" >&2
    exit 1
fi
echo "[build] 1/6 arch OK: aarch64"

# All intermediates (zip, extracted tree, whl, go tarball) live under $WORK,
# removed on exit — always created so later download steps can rely on it.
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# ---------------------------------------------------------------------------
# 2. dyno / dynolog from the msmonitor bundle (skip when already present)
# ---------------------------------------------------------------------------
if [ -s "$BIN_DIR/dyno" ] && [ -s "$BIN_DIR/dynolog" ]; then
    echo "[build] 2/6 dyno/dynolog already present in $BIN_DIR, skipping download"
else
    command -v wget >/dev/null 2>&1 || { echo "[build] ERROR: wget not found" >&2; exit 1; }
    command -v unzip >/dev/null 2>&1 || { echo "[build] ERROR: unzip not found" >&2; exit 1; }

    ZIP_URL="https://ptdbg.obs.cn-north-4.myhuaweicloud.com/profiler/msmonitor/8.1.0/aarch64_8.1.0.zip"
    echo "[build] 2/6 downloading $ZIP_URL"
    wget "${WGET_ARGS[@]}" -O "$WORK/aarch64_8.1.0.zip" "$ZIP_URL"
    echo "[build]     unzipping..."
    unzip -q "$WORK/aarch64_8.1.0.zip" -d "$WORK/extracted"

    DYNO_SRC=$(find "$WORK/extracted" -type f -path '*/bin/dyno' | head -n 1 || true)
    DYNOLOG_SRC=$(find "$WORK/extracted" -type f -path '*/bin/dynolog' | head -n 1 || true)
    if [ -z "$DYNO_SRC" ] || [ -z "$DYNOLOG_SRC" ]; then
        echo "[build] ERROR: dyno/dynolog not found under $WORK/extracted/bin" >&2
        exit 1
    fi
    mkdir -p "$BIN_DIR"
    install -m 0755 "$DYNO_SRC" "$BIN_DIR/dyno"
    install -m 0755 "$DYNOLOG_SRC" "$BIN_DIR/dynolog"
    echo "[build]     dyno/dynolog -> $BIN_DIR/"
fi

# ---------------------------------------------------------------------------
# 3. Python version: must be 3.9 / 3.10 / 3.11 / 3.12
# ---------------------------------------------------------------------------
PY=""
for c in python3 python; do
    if command -v "$c" >/dev/null 2>&1; then PY="$c"; break; fi
done
if [ -z "$PY" ]; then
    echo "[build] ERROR: python not found" >&2
    exit 1
fi
PYVER=$("$PY" -c 'import sys; print("%d.%d" % sys.version_info[:2])')
case "$PYVER" in
    3.9)  CP="cp39" ;;
    3.10) CP="cp310" ;;
    3.11) CP="cp311" ;;
    3.12) CP="cp312" ;;
    *)
        echo "[build] ERROR: python $PYVER not supported (need 3.9, 3.10, 3.11 or 3.12)" >&2
        exit 1
        ;;
esac
echo "[build] 3/6 python $PYVER OK (wheel tag $CP)"

# ---------------------------------------------------------------------------
# 4. mindstudio_monitor wheel (skip when 26.2.0 already installed)
# ---------------------------------------------------------------------------
INSTALLED_VER=$("$PY" -m pip show mindstudio_monitor 2>/dev/null \
    | awk -F': ' '/^Version:/{print $2}' | head -n 1 || true)
if [ "$INSTALLED_VER" = "26.2.0" ]; then
    echo "[build] 4/6 mindstudio_monitor 26.2.0 already installed, skipping"
else
    command -v wget >/dev/null 2>&1 || { echo "[build] ERROR: wget not found" >&2; exit 1; }
    WHL_URL="https://mindstudio-pkg.obs.cn-north-4.myhuaweicloud.com/tag/26.2.0/B025/aarch64/mindstudio_monitor-26.2.0-${CP}-${CP}-linux_aarch64.whl"
    echo "[build] 4/6 downloading $WHL_URL"
    wget "${WGET_ARGS[@]}" -O "$WORK/mindstudio_monitor-26.2.0-${CP}-${CP}-linux_aarch64.whl" "$WHL_URL"
    "$PY" -m pip install "$WORK/mindstudio_monitor-26.2.0-${CP}-${CP}-linux_aarch64.whl"
fi

# ---------------------------------------------------------------------------
# 5. Go toolchain: require go.mod's version, fetch from Aliyun when missing/old
# ---------------------------------------------------------------------------
GO_VERSION_REQ=$(grep -E '^go ' go.mod | awk '{print $2}' | head -n 1 || true)
[ -z "$GO_VERSION_REQ" ] && GO_VERSION_REQ="1.23.4"

# Locate go: PATH first, then the install dirs build.sh itself uses
# (/usr/local/go, ~/.local/go). Probing known dirs makes a previous run's
# install visible even when the PATH export below is not persisted in the
# current shell, so an adequate go is never re-downloaded.
GO_CANDS=()
if command -v go >/dev/null 2>&1; then GO_CANDS+=("$(command -v go)"); fi
GO_CANDS+=(/usr/local/go/bin/go "$HOME/.local/go/bin/go")

GO_OK=0
GO_BIN=""
GO_INSTALLED=""
for c in "${GO_CANDS[@]}"; do
    [ -x "$c" ] || continue
    v=$("$c" version 2>/dev/null | grep -oE 'go[0-9]+\.[0-9]+(\.[0-9]+)?' | head -n 1 | sed 's/^go//' || true)
    if [ -n "$v" ] && ver_ge "$v" "$GO_VERSION_REQ"; then
        GO_BIN="$c"; GO_INSTALLED="$v"; break
    fi
done
if [ -n "$GO_BIN" ]; then
    echo "[build] 5/6 go $GO_INSTALLED >= $GO_VERSION_REQ OK ($GO_BIN)"
    GO_OK=1
else
    echo "[build] 5/6 no go >= $GO_VERSION_REQ (PATH / /usr/local/go / ~/.local/go), downloading from Aliyun"
fi

if [ "$GO_OK" -ne 1 ]; then
    # Prefer the standard /usr/local/go; fall back to a user-local dir when it
    # is not writable and there is no sudo.
    GO_ROOT=/usr/local/go
    if [ ! -w /usr/local ] && ! command -v sudo >/dev/null 2>&1; then
        GO_ROOT="$HOME/.local/go"
    fi
    command -v wget >/dev/null 2>&1 || { echo "[build] ERROR: wget not found" >&2; exit 1; }
    GO_URL="https://mirrors.aliyun.com/golang/go${GO_VERSION_REQ}.linux-arm64.tar.gz"
    echo "[build]     downloading $GO_URL"
    wget "${WGET_ARGS[@]}" -O "$WORK/go.tar.gz" "$GO_URL"

    echo "[build]     installing Go $GO_VERSION_REQ -> $GO_ROOT"
    if [ -d "$GO_ROOT" ]; then
        rm -rf "$GO_ROOT"
    fi
    mkdir -p "$(dirname "$GO_ROOT")"
    if [ -w "$(dirname "$GO_ROOT")" ]; then
        tar -C "$(dirname "$GO_ROOT")" -xzf "$WORK/go.tar.gz"
    elif command -v sudo >/dev/null 2>&1; then
        sudo rm -rf "$GO_ROOT"
        sudo tar -C "$(dirname "$GO_ROOT")" -xzf "$WORK/go.tar.gz"
    else
        echo "[build] ERROR: cannot write to $(dirname "$GO_ROOT") and no sudo" >&2
        exit 1
    fi
    export PATH="$GO_ROOT/bin:$PATH"
    echo "[build]     go $(go version | grep -oE 'go[0-9]+\.[0-9]+(\.[0-9]+)?')"

    # Persist the PATH export in the user's shell rc so future interactive
    # shells also find go. Marked and guarded: appended only once. build.sh
    # itself does not rely on this — the known-dir probe above already finds
    # the freshly installed go on the next run.
    case "${SHELL:-}" in
        *zsh*) RC="$HOME/.zshrc" ;;
        *bash*) RC="$HOME/.bashrc" ;;
        *) RC="$HOME/.profile" ;;
    esac
    if [ -w "$HOME" ] && ! grep -qs 'CATHelper/straggler build.sh: Go PATH' "$RC"; then
        {
            echo ""
            echo "# >>> CATHelper/straggler build.sh: Go PATH >>>"
            echo "export PATH=\"$GO_ROOT/bin:\$PATH\""
            echo "# <<< CATHelper/straggler build.sh: Go PATH <<<"
        } >> "$RC" 2>/dev/null || true
        if grep -qs 'CATHelper/straggler build.sh: Go PATH' "$RC"; then
            echo "[build]     persisted 'export PATH=$GO_ROOT/bin:\$PATH' to $RC"
        else
            echo "[build]     WARNING: could not persist PATH to $RC" >&2
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 6. go build
# ---------------------------------------------------------------------------
echo "[build] 6/6 go build"
CGO_ENABLED=0 go build -o slowNodeDetection .
echo "[build] done: ./slowNodeDetection"
