#!/usr/bin/env bash
# Deploy backend LiveKit: tarik kode terbaru, build, jalankan/-ulang via PM2.
set -euo pipefail
cd "$(dirname "$0")"

echo "==> git pull"
git pull --ff-only

echo "==> go build"
export PATH="$PATH:/usr/local/go/bin"
CGO_ENABLED=0 go build -trimpath -o livekit-be-server ./cmd/server

set -a; [ -f /opt/apps/livekit.env ] && . /opt/apps/livekit.env; set +a

# ---------------------------------------------------------------------------
# Cek kredensial LiveKit. Tanpa LIVEKIT_URL + API key/secret, service tetap
# hidup tapi seluruh endpoint token/room/egress menolak — gejalanya di FE
# terlihat seperti "tombol join tidak jalan", bukan seperti service mati.
# ---------------------------------------------------------------------------
cek_livekit() {
  if [ -z "${LIVEKIT_API_KEY:-}" ] || [ -z "${LIVEKIT_API_SECRET:-}" ]; then
    echo "    !! LIVEKIT_API_KEY/LIVEKIT_API_SECRET belum diisi di /opt/apps/livekit.env"
    echo "       Butuh minimal tiga baris:"
    echo "         LIVEKIT_URL=wss://<project>.livekit.cloud"
    echo "         LIVEKIT_API_KEY=..."
    echo "         LIVEKIT_API_SECRET=...   (rahasia, min 32 karakter)"
    return 0
  fi
  if [ "${#LIVEKIT_API_SECRET}" -lt 32 ]; then
    echo "    !! LIVEKIT_API_SECRET kurang dari 32 karakter — terlalu lemah untuk produksi."
  fi
  echo "    kredensial LiveKit OK -> ${LIVEKIT_URL:-(LIVEKIT_URL belum di-set!)}"
}
echo "==> cek kredensial"
cek_livekit

echo "==> (re)start PM2: livekit-be"
pm2 restart livekit-be --update-env 2>/dev/null || pm2 start ./livekit-be-server --name livekit-be --update-env
pm2 save
echo "==> selesai. status:"
pm2 status livekit-be
