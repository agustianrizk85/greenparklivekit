package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"greenpark/livekit/internal/domain"
)

// Kode akses tamu: tautan saja tidak lagi cukup. Tautan rapat gampang
// diteruskan ke grup obrolan, dan token panjang di dalamnya sudah memberi jalan
// masuk — kode inilah faktor kedua yang dikirim lewat jalur berbeda.
func TestKodeAksesTamu(t *testing.T) {
	svc := newSvc(t)
	ctx := context.Background()
	host := domain.User{ID: "u1", Username: "budi", Name: "Budi", Roles: map[string]string{"teknik": "admin"}}

	m, err := svc.CreateMeeting(ctx, host, MeetingInput{Title: "Rapat Konsumen C-07", Kind: domain.KindMeeting})
	if err != nil {
		t.Fatalf("buat rapat: %v", err)
	}

	_, token, kode, err := svc.GuestLink(host, m.ID, true)
	if err != nil {
		t.Fatalf("nyalakan tautan tamu: %v", err)
	}
	if token == "" {
		t.Fatal("token tamu kosong")
	}
	if len(kode) != guestCodeDigits || strings.Trim(kode, "0123456789") != "" {
		t.Fatalf("kode mau %d angka, dapat %q", guestCodeDigits, kode)
	}

	// Halaman tamu harus TAHU kode diperlukan, tapi TIDAK boleh diberi kodenya —
	// halaman itu publik.
	info, err := svc.GuestMeeting(token)
	if err != nil {
		t.Fatalf("info tamu: %v", err)
	}
	if !info.CodeRequired {
		t.Fatal("info tamu seharusnya menyatakan kode diperlukan")
	}

	// Kode salah ditolak.
	if _, err = svc.JoinAsGuest(ctx, token, "Pak Joko", "000000"); err == nil {
		t.Fatal("kode salah seharusnya ditolak")
	} else if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mau ErrValidation, dapat %v", err)
	}
	// Kode kosong juga ditolak — bukan dianggap "tautan tanpa kode".
	if _, err = svc.JoinAsGuest(ctx, token, "Pak Joko", ""); err == nil {
		t.Fatal("kode kosong seharusnya ditolak saat rapat memakai kode")
	}

	// Kode benar LOLOS pemeriksaan kode. Server LiveKit tidak hidup di lingkungan
	// tes, jadi yang diperiksa bukan "berhasil masuk" melainkan "tidak lagi
	// ditolak KARENA KODENYA" -- itulah yang sedang diuji di sini.
	if _, err = svc.JoinAsGuest(ctx, token, "Pak Joko", kode); !lolosKode(err) {
		t.Fatalf("kode benar seharusnya lolos pemeriksaan kode, dapat: %v", err)
	}
}

// Penebakan beruntun dihentikan: enam angka bisa dihabiskan skrip kalau boleh
// dicoba tanpa batas.
func TestPercobaanKodeDibatasi(t *testing.T) {
	svc := newSvc(t)
	ctx := context.Background()
	host := domain.User{ID: "u1", Username: "budi", Roles: map[string]string{"teknik": "admin"}}
	m, _ := svc.CreateMeeting(ctx, host, MeetingInput{Title: "Rapat Uji", Kind: domain.KindMeeting})
	_, token, kode, err := svc.GuestLink(host, m.ID, true)
	if err != nil {
		t.Fatalf("tautan tamu: %v", err)
	}

	salah := "000000"
	if salah == kode {
		salah = "111111"
	}
	for i := 0; i < maxGuestCodeAttempts; i++ {
		if _, err := svc.JoinAsGuest(ctx, token, "Penebak", salah); err == nil {
			t.Fatal("kode salah seharusnya ditolak")
		}
	}
	// Setelah batas terlampaui, KODE YANG BENAR pun ditolak sementara — kalau
	// tidak, pembatasnya tidak menghentikan apa pun.
	_, err = svc.JoinAsGuest(ctx, token, "Pak Joko", kode)
	if err == nil {
		t.Fatal("setelah percobaan habis, tautan seharusnya didinginkan")
	}
	if !strings.Contains(err.Error(), "terlalu banyak percobaan") {
		t.Fatalf("mau pesan pendinginan, dapat %v", err)
	}

	// Merotasi tautan MELUPAKAN hitungan itu — tautan baru tidak boleh lahir
	// dalam keadaan sudah didinginkan.
	_, token2, kode2, err := svc.GuestLink(host, m.ID, true)
	if err != nil {
		t.Fatalf("rotasi tautan: %v", err)
	}
	if _, err := svc.JoinAsGuest(ctx, token2, "Pak Joko", kode2); !lolosKode(err) {
		t.Fatalf("tautan baru seharusnya tidak didinginkan, dapat: %v", err)
	}
	// Token lama mati begitu dirotasi.
	if _, err := svc.JoinAsGuest(ctx, token, "Pak Joko", kode); err == nil {
		t.Fatal("token lama seharusnya mati setelah rotasi")
	}
}

// Tautan LAMA yang dibuat sebelum kode ada tetap berlaku tanpa kode — kalau
// tidak, setiap tautan yang sudah tersebar mendadak mati.
func TestTautanLamaTanpaKodeTetapBerlaku(t *testing.T) {
	svc := newSvc(t)
	ctx := context.Background()
	host := domain.User{ID: "u1", Username: "budi", Roles: map[string]string{"teknik": "admin"}}
	m, _ := svc.CreateMeeting(ctx, host, MeetingInput{Title: "Rapat Lama", Kind: domain.KindMeeting})
	_, token, _, err := svc.GuestLink(host, m.ID, true)
	if err != nil {
		t.Fatalf("tautan tamu: %v", err)
	}
	// Tiru keadaan lama: kodenya dikosongkan di penyimpanan.
	if _, err := svc.st.UpdateMeeting(m.ID, func(mm *domain.Meeting) error {
		mm.GuestCode = ""
		return nil
	}); err != nil {
		t.Fatalf("kosongkan kode: %v", err)
	}
	info, err := svc.GuestMeeting(token)
	if err != nil {
		t.Fatalf("info tamu: %v", err)
	}
	if info.CodeRequired {
		t.Fatal("tautan tanpa kode tidak boleh meminta kode")
	}
	if _, err := svc.JoinAsGuest(ctx, token, "Pak Joko", ""); !lolosKode(err) {
		t.Fatalf("tautan lama seharusnya tidak diminta kode, dapat: %v", err)
	}
}

// lolosKode melaporkan apakah percobaan masuk BERHASIL MELEWATI pemeriksaan
// kode. Server LiveKit tidak dijalankan di lingkungan tes, jadi jalur sukses
// selalu berhenti saat menyiapkan room — kegagalan itu justru BUKTI kodenya
// sudah lolos, karena pemeriksaan kode terjadi sebelum room disentuh.
func lolosKode(err error) bool {
	if err == nil {
		return true
	}
	m := err.Error()
	return !strings.Contains(m, "kode akses salah") && !strings.Contains(m, "terlalu banyak percobaan")
}
