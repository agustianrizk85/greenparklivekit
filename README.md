# greenparklivekitbe — LiveKit (WebRTC) API

Backend Go untuk fitur rapat/video call Greenpark di atas **LiveKit**. Tugasnya:
menerbitkan **access token** WebRTC, mengelola **room & meeting**, menjalankan
**egress** (rekam ke file / streaming RTMP), menerima **webhook** dari LiveKit,
dan **dispatch voice AI agent** ke dalam room.

Autentikasi memakai **SSO Greenpark** (Master Auth `:8090`) — sama seperti
backend divisi lain: token dari dashboard diverifikasi lewat JWKS, tidak ada
login lokal sendiri.

## Jalankan

**Lokal (dev)**:

```powershell
copy .env.example .env    # lalu isi LIVEKIT_API_KEY / LIVEKIT_API_SECRET
./run.ps1                 # = go run ./cmd/server
```

Atau langsung: `go run ./cmd/server`.

**Server (produksi)** — sama seperti backend lain: `./deploy.sh`
(git pull → `go build -o livekit-be-server` → PM2 `livekit-be`).
Env di luar git: `/opt/apps/livekit.env`.

Port default **8089**. Health: `GET /api/health`.

### Env

Daftar lengkap beserta penjelasannya ada di [`.env.example`](.env.example).
Yang wajib diisi: `LIVEKIT_URL`, `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`.

## API

| Method | Path | Akses | Keterangan |
|---|---|---|---|
| GET | `/api/health` | publik | cek service hidup |
| GET | `/api/config` | auth | URL LiveKit + daftar fitur yang aktif |
| GET | `/api/auth/me` | auth | identitas user dari SSO |
| GET | `/api/meetings` | auth | daftar meeting |
| POST | `/api/meetings` | auth | buat meeting baru |
| GET | `/api/meetings/{id}` | auth | detail meeting |
| PATCH / PUT | `/api/meetings/{id}` | host/direktur | ubah meeting |
| DELETE | `/api/meetings/{id}` | host/direktur | hapus meeting |
| POST | `/api/meetings/{id}/join` | auth | gabung — balikan `{token,url,room,role}` |
| POST | `/api/meetings/{id}/start` | host | mulai meeting |
| POST | `/api/meetings/{id}/end` | host | akhiri meeting |
| GET | `/api/meetings/{id}/participants` | auth | daftar peserta |
| POST | `/api/meetings/{id}/kick` | host | keluarkan peserta |
| POST | `/api/meetings/{id}/mute` | host | bisukan peserta |
| GET | `/api/meetings/{id}/egress` | auth | daftar egress room (`?active=1` = yang jalan saja) |
| POST | `/api/meetings/{id}/record/start` | host | mulai rekam (egress file) |
| POST | `/api/meetings/{id}/record/stop` | host | stop rekam |
| POST | `/api/meetings/{id}/stream/start` | host | mulai streaming RTMP |
| POST | `/api/meetings/{id}/stream/stop` | host | stop streaming |
| POST | `/api/meetings/{id}/agent` | host | dispatch voice AI agent ke room |
| POST | `/api/token` | auth | token ad-hoc (di luar alur meeting) |
| GET | `/api/rooms` | direktur | daftar room aktif di server LiveKit |
| DELETE | `/api/rooms/{name}` | direktur | bubarkan room |
| GET | `/api/events` | auth | riwayat event (join/leave/rekam/dll) |
| GET | `/api/ws` | realtime | push update; token lewat `?token=` |
| POST | `/api/livekit/webhook` | publik | dari server LiveKit, diverifikasi tanda tangan |

### Aturan akses

Layanan ini **lintas divisi**: siapa pun yang punya peran di divisi mana pun
(atau `super`) boleh memakai API-nya — tidak perlu peran khusus "livekit".
Peran di dalam room ditentukan berurutan:

1. pembuat meeting → `host` (boleh publish + kendali room)
2. terdaftar di `invitees` → peran sesuai undangan (`host`/`speaker`/`viewer`/`agent`)
3. `ceo` / `dirops` / `admin` / `super` → `host` (akses direksi lintas divisi)
4. `open: true` atau divisinya termasuk di `divisions` → `speaker`
   (khusus `kind: "stream"` → `viewer`, hanya menonton)
5. selain itu → ditolak (403)

`kind` menentukan peruntukan room: `meeting` (rapat dua arah), `stream`
(siaran/share layar satu-ke-banyak), `voiceai` (sesi dengan agent suara).

## Menyiapkan LiveKit

Pilih salah satu.

**(a) LiveKit Cloud** — paling cepat, tidak perlu ngurus server media.

1. Daftar di [livekit.io](https://livekit.io), buat project.
2. Ambil **URL** (`wss://<project>.livekit.cloud`) + **API key** & **secret**.
3. Isi ke `.env`: `LIVEKIT_URL`, `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`.
4. Di dashboard LiveKit, set **webhook URL** ke `https://<host>/api/livekit/webhook`.

**(b) Self-hosted / dev lokal**:

```bash
livekit-server --dev
```

Mode dev memakai key `devkey` / secret `secret` dan listen di `ws://localhost:7880`.
Catatan: **Egress** (rekam & streaming RTMP) butuh service `livekit/egress`
terpisah beserta **Redis**, dan **tidak tersedia** di mode dev — endpoint
`record/*` dan `stream/*` tidak akan berfungsi sampai egress dipasang.

## Integrasi frontend

FE `dashboard/fe` tidak perlu tahu API key LiveKit sama sekali. Cukup panggil:

```
POST /api/meetings/{id}/join   ->  { token, url, room, role }
```

lalu pakai `livekit-client` / `@livekit/components-react` di browser untuk
connect ke `url` memakai `token` tersebut.

Env FE yang disarankan:

```
VITE_LIVEKIT_API=http://localhost:8089
```

## Catatan

- Data (meeting, peserta, event) disimpan di file JSON `data/livekit-data.json` —
  **belum** Postgres. Jangan di-commit; folder `data/` sudah di-gitignore.
- Endpoint webhook **wajib bisa diakses publik** oleh server LiveKit, kalau tidak
  status room/peserta tidak akan pernah ter-update otomatis.
- Token join berumur **4 jam** (`LIVEKIT_TOKEN_TTL_MIN=240`) — peserta yang
  membuka tab lebih lama dari itu harus join ulang untuk dapat token baru.
