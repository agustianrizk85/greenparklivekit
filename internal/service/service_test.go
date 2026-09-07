package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"greenpark/livekit/internal/domain"
	"greenpark/livekit/internal/lk"
	"greenpark/livekit/internal/store"
)

func newSvc(t *testing.T) *Service {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "livekit-data.json"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	c := lk.New(lk.Config{URL: "ws://localhost:7880", APIKey: "devkey", APISecret: "secretsecretsecretsecretsecret32", TokenTTL: time.Hour})
	if !c.Enabled() {
		t.Fatal("klien livekit seharusnya aktif")
	}
	return New(st, c, Options{RecordDir: "recordings", TokenTTL: time.Hour})
}

func claimsOf(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token bukan JWT: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return m
}

func TestMeetingLifecycleDanToken(t *testing.T) {
	svc := newSvc(t)
	ctx := context.Background()
	host := domain.User{ID: "u1", Username: "budi", Name: "Budi", Roles: map[string]string{"teknik": "admin"}}
	tamu := domain.User{ID: "u2", Username: "sari", Name: "Sari", Roles: map[string]string{"marketing": "staff"}}

	m, err := svc.CreateMeeting(ctx, host, MeetingInput{Title: "Rapat Progres Blok A!", Kind: domain.KindMeeting})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Nama room = slug judul + akhiran acak (sejak room dibuat selalu unik, agar
	// dua rapat berjudul sama tidak berbagi satu room). Yang diuji karena itu
	// AWALANNYA, bukan kesamaan persis — asersi lama tertinggal saat akhiran itu
	// ditambahkan dan membuat berkas tes ini merah.
	if !strings.HasPrefix(m.Room, "rapat-progres-blok-a-") {
		t.Fatalf("slug room salah: %q", m.Room)
	}
	if m.HostID != "u1" || m.Division != "teknik" {
		t.Fatalf("host/divisi salah: %+v", m)
	}

	// Tamu di luar divisi & bukan undangan: tidak boleh melihat/bergabung.
	if _, ok := m.RoleFor(tamu); ok {
		t.Fatal("tamu seharusnya belum berhak")
	}
	if _, err := svc.GetMeeting(tamu, m.ID); err != domain.ErrForbidden {
		t.Fatalf("harusnya ErrForbidden, dapat %v", err)
	}

	// Undang tamu sebagai speaker.
	updated, err := svc.UpdateMeeting(host, m.ID, MeetingInput{
		Invitees: []domain.Invitee{{UserID: "u2", Username: "sari", Role: domain.RoleSpeaker}},
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if role, ok := updated.RoleFor(tamu); !ok || role != domain.RoleSpeaker {
		t.Fatalf("peran tamu salah: %v %v", role, ok)
	}
	if updated.CanManage(tamu) {
		t.Fatal("tamu tidak boleh mengelola")
	}

	// Token ad-hoc (tidak menyentuh server LiveKit).
	res, err := svc.AdHocToken(ctx, host, TokenRequest{Room: "Rapat Bebas", Role: domain.RoleHost})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if res.Room != "rapat-bebas" || res.Token == "" || res.URL != "ws://localhost:7880" {
		t.Fatalf("hasil token janggal: %+v", res)
	}
	cl := claimsOf(t, res.Token)
	video, _ := cl["video"].(map[string]any)
	if video["room"] != "rapat-bebas" || video["roomJoin"] != true {
		t.Fatalf("grant salah: %v", video)
	}
	if video["canPublish"] != true || video["roomAdmin"] != true {
		t.Fatalf("host harus boleh publish + admin: %v", video)
	}
	if cl["sub"] != "budi" {
		t.Fatalf("identity salah: %v", cl["sub"])
	}

	// Meeting terdaftar: token ad-hoc harus jatuh ke kebijakan meeting
	// (di sini gagal karena server LiveKit tidak berjalan — bukan diizinkan diam-diam).
	if _, err := svc.AdHocToken(ctx, tamu, TokenRequest{Room: m.Room}); err == nil {
		t.Fatal("harusnya gagal karena butuh server LiveKit untuk menyiapkan room")
	}

	// Webhook memutakhirkan status meeting.
	svc.HandleWebhook(lk.WebhookEvent{Event: "room_started", Room: m.Room})
	got, err := svc.GetMeeting(host, m.ID)
	if err != nil || got.Status != domain.StatusLive {
		t.Fatalf("status setelah room_started: %v %v", got.Status, err)
	}
	svc.HandleWebhook(lk.WebhookEvent{Event: "participant_joined", Room: m.Room, NumParticipants: 3})
	svc.HandleWebhook(lk.WebhookEvent{Event: "room_finished", Room: m.Room})
	got, _ = svc.GetMeeting(host, m.ID)
	if got.Status != domain.StatusEnded || got.EndedAt == nil {
		t.Fatalf("status setelah room_finished: %+v", got)
	}
	if ev := svc.Events(host, m.Room, 10); len(ev) < 3 {
		t.Fatalf("event kurang: %d", len(ev))
	}

	// Streaming: hanya pengelola, dan wajib ada URL.
	if _, err := svc.StartStream(ctx, tamu, m.ID, "grid", []string{"rtmp://x/y"}); err != domain.ErrForbidden {
		t.Fatalf("tamu tidak boleh streaming, dapat %v", err)
	}
	if _, err := svc.StartStream(ctx, host, m.ID, "grid", nil); err == nil || !strings.Contains(err.Error(), "URL RTMP") {
		t.Fatalf("harusnya menolak tanpa URL, dapat %v", err)
	}
}

func TestAlamatUntukBrowserTerpisahDariAlamatInternal(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "d.json"))
	if err != nil {
		t.Fatal(err)
	}
	c := lk.New(lk.Config{URL: "ws://127.0.0.1:7880", APIKey: "devkey", APISecret: "secretsecretsecretsecretsecret32", TokenTTL: time.Hour})
	u := domain.User{ID: "u1", Username: "budi", Roles: map[string]string{"teknik": "admin"}}

	// Tanpa PublicURL: browser diberi alamat yang sama dengan yang dipakai service.
	svc := New(st, c, Options{TokenTTL: time.Hour})
	if got := svc.Config().URL; got != "ws://127.0.0.1:7880" {
		t.Fatalf("tanpa PublicURL harusnya alamat internal, dapat %q", got)
	}

	// Dengan PublicURL (kasus self-host di balik reverse proxy): browser diberi
	// alamat wss publik, bukan 127.0.0.1 yang pasti gagal dari luar.
	svc = New(st, c, Options{TokenTTL: time.Hour, PublicURL: "wss://contoh.id/livekit"})
	if got := svc.Config().URL; got != "wss://contoh.id/livekit" {
		t.Fatalf("Config().URL salah: %q", got)
	}
	res, err := svc.AdHocToken(context.Background(), u, TokenRequest{Room: "rapat"})
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if res.URL != "wss://contoh.id/livekit" {
		t.Fatalf("URL di hasil join salah: %q", res.URL)
	}
}

func TestKlienNonaktifMenolakDenganPesanJelas(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "d.json"))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, lk.New(lk.Config{URL: "ws://localhost:7880"}), Options{})
	u := domain.User{ID: "u1", Username: "budi", Roles: map[string]string{"sales": "admin"}}
	if _, err := svc.AdHocToken(context.Background(), u, TokenRequest{Room: "abc"}); err != domain.ErrDisabled {
		t.Fatalf("harusnya ErrDisabled, dapat %v", err)
	}
	if cfg := svc.Config(); cfg.Enabled {
		t.Fatal("Config().Enabled harus false")
	}
}
