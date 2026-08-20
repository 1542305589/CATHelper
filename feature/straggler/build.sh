#!/usr/bin/env bash
# Provision the straggler build dependencies on an aarch64 host and compile:
#   1. check arch == aarch64 (exit otherwise)
#   2. fetch dyno/dynolog from the msmonitor 8.1.0 bundle zip
#   3. check python version is in {3.9, 3.10, 3.11, 3.12}
#   4. pip install the mindstudio_monitor wheel (cp tag matched to python)
#   5. go build
#
# Run:  bash build.sh
set -euo pipefail
cd "$(dirname "$0")"

BIN_DIR="3rdparty/bin"

# ---------------------------------------------------------------------------
# 1. Architecture: only aarch64 is supported
# ---------------------------------------------------------------------------
ARCH=$(uname -m)
if [ "$ARCH" != "aarch64" ]; then
    echo "[build] ERROR: unsupported architecture '$ARCH' — only aarch64 is supported" >&2
    exit 1
fi
echo "[build] 1/5 arch OK: aarch64"

# ---------------------------------------------------------------------------
# 2. dyno / dynolog from the msmonitor bundle
# ---------------------------------------------------------------------------
command -v wget >/dev/null 2>&1 || { echo "[build] ERROR: wget not found" >&2; exit 1; }
command -v unzip >/dev/null 2>&1 || { echo "[build] ERROR: unzip not found" >&2; exit 1; }

# All intermediates (zip, extracted tree, whl) live under $WORK, removed on exit.
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

ZIP_URL="https://ptdbg.obs.cn-north-4.myhuaweicloud.com/profiler/msmonitor/8.1.0/aarch64_8.1.0.zip"
echo "[build] 2/5 downloading $ZIP_URL"
wget -O "$WORK/aarch64_8.1.0.zip" "$ZIP_URL"
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
echo "[build] 3/5 python $PYVER OK (wheel tag $CP)"

# ---------------------------------------------------------------------------
# 4. mindstudio_monitor wheel (version pinned to the python's cp tag)
# ---------------------------------------------------------------------------
WHL_URL="https://mindstudio-pkg.obs.cn-north-4.myhuaweicloud.com/tag/26.2.0/B025/aarch64/mindstudio_monitor-26.2.0-${CP}-${CP}-linux_aarch64.whl"
echo "[build] 4/5 downloading $WHL_URL"
wget -O "$WORK/mindstudio_monitor.whl" "$WHL_URL"
"$PY" -m pip install "$WORK/mindstudio_monitor.whl"

# ---------------------------------------------------------------------------
# 5. go build
# ---------------------------------------------------------------------------
echo "[build] 5/5 go build"
CGO_ENABLED=0 go build -o slowNodeDetection .
echo "[build] done: ./slowNodeDetection"
