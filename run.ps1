# Jalankan backend LiveKit Greenpark untuk dev lokal.
#
# Semua env di bawah hanya di-set kalau BELUM ada nilainya, jadi kamu bisa
# meng-override dari shell (atau lewat .env) tanpa mengubah file ini. Run:  ./run.ps1
$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Port HTTP service (8080 dipakai Apache — jangan diubah ke situ).
if (-not $env:LIVEKIT_BE_PORT) { $env:LIVEKIT_BE_PORT = "8089" }

# SSO Greenpark (Master Auth :8090) — token dashboard diverifikasi lewat JWKS.
if (-not $env:AUTH_JWKS_URL)   { $env:AUTH_JWKS_URL = "http://localhost:8090/.well-known/jwks.json" }
if (-not $env:AUTH_ISSUER)     { $env:AUTH_ISSUER   = "greenpark-auth" }

# Server LiveKit. Default menunjuk ke `livekit-server --dev` di mesin sendiri.
if (-not $env:LIVEKIT_URL)     { $env:LIVEKIT_URL   = "ws://localhost:7880" }

# Kredensial LiveKit tidak diberi default: secret adalah rahasia dan tidak boleh
# ada di file yang ter-commit. Service tetap dijalankan supaya /api/health dan
# endpoint non-LiveKit bisa dites, tapi token/room akan ditolak.
if (-not $env:LIVEKIT_API_KEY -or -not $env:LIVEKIT_API_SECRET) {
  Write-Warning "LIVEKIT_API_KEY/LIVEKIT_API_SECRET kosong — endpoint token, room, egress, dan dispatch agent akan MENOLAK permintaan."
  Write-Warning "Isi lewat .env (lihat .env.example). Untuk 'livekit-server --dev' pakai devkey/secret."
}

$adaKey = if ($env:LIVEKIT_API_KEY) { "ada" } else { "KOSONG" }
Write-Host "livekit-be: port=$env:LIVEKIT_BE_PORT  livekit=$env:LIVEKIT_URL  api-key=$adaKey"
Write-Host "livekit-be: sso=$env:AUTH_JWKS_URL  issuer=$env:AUTH_ISSUER"
go run ./cmd/server
