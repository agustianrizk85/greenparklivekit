package service

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"time"

	"greenpark/livekit/internal/domain"
	"greenpark/livekit/internal/lk"
)

/* ════════════════════════════════════════════════════════════════════════
 * TAUTAN TAMU — masuk rapat TANPA akun Greenpark.
 *
 * Ini satu-satunya jalur di layanan ini yang tidak melewati SSO, jadi seluruh
 * pembatasannya dikumpulkan di berkas ini agar bisa ditinjau sekaligus:
 *
 *   1. Token acak 32 byte. Ia PENGGANTI kata sandi, jadi harus tidak bisa
 *      ditebak — bukan id meeting yang berurutan atau nama room yang terbaca.
 *   2. Tamu SELALU non-admin. Perannya dipaksa speaker (atau viewer untuk
 *      siaran); tidak pernah host. Jadi tamu tidak bisa mengeluarkan peserta,
 *      membisukan, merekam, atau mengakhiri rapat — walau ia mengarang isi
 *      permintaannya.
 *   3. Bisa dimatikan kapan saja. Mematikan tautan MENGHAPUS tokennya, jadi
 *      tautan yang sudah tersebar langsung mati; menyalakannya lagi menerbitkan
 *      token baru, bukan menghidupkan yang lama.
 *   4. Rapat yang sudah berakhir menolak tamu, sama seperti peserta internal.
 *
 * Yang SENGAJA tidak dilakukan: tamu tidak dicatat sebagai invitee dan tidak
 * pernah bisa menjadi host lewat aturan `Open`/`Divisions` — jalur tamu punya
 * penentuan peran sendiri (guestRole), terpisah dari RoleFor.
 */

// guestTokenBytes = 32 byte (256 bit) — sekelas kunci sesi, bukan kode undangan
// pendek yang bisa dicoba satu per satu.
const guestTokenBytes = 32

// maxGuestNameLen membatasi nama tampilan tamu. Nama datang dari orang luar dan
// tampil di layar semua peserta; tanpa batas ia bisa dipakai merusak tata letak.
const maxGuestNameLen = 60

// guestCodeDigits = panjang kode pendek yang diketik tamu. Enam angka dipilih
// supaya masih bisa didiktekan lewat telepon; keamanannya TIDAK bersandar pada
// panjangnya, melainkan pada (a) token 32 byte di tautan yang harus dipegang
// lebih dulu, dan (b) pembatas percobaan di bawah.
const guestCodeDigits = 6

// maxGuestCodeAttempts membatasi percobaan kode SALAH per token sebelum tautan
// itu berhenti melayani sementara.
//
// Tanpa ini, kode enam angka bisa ditebak habis oleh siapa pun yang memegang
// tautannya — sejuta percobaan bukan penghalang bagi skrip. Hitungannya per
// TOKEN (bukan per IP): yang dilindungi adalah rapatnya, dan pengganti IP
// gampang sekali.
const maxGuestCodeAttempts = 10

// guestCodeCooldown = lama tautan didinginkan setelah percobaan habis.
const guestCodeCooldown = 15 * time.Minute

// newGuestCode membuat kode numerik acak. rand kriptografis, bukan pseudo —
// kode yang bisa diramalkan sama saja dengan tidak ada kode.
func newGuestCode() (string, error) {
	const digits = "0123456789"
	b := make([]byte, guestCodeDigits)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("gagal membuat kode tamu: %w", err)
	}
	out := make([]byte, guestCodeDigits)
	for i, v := range b {
		out[i] = digits[int(v)%len(digits)]
	}
	return string(out), nil
}

func newGuestToken() (string, error) {
	b := make([]byte, guestTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("gagal membuat token tamu: %w", err)
	}
	// URL-safe: token ini hidup di dalam tautan yang disalin-tempel orang.
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// bersihkanNamaTamu merapikan nama yang diketik tamu.
func bersihkanNamaTamu(s string) string {
	s = strings.TrimSpace(s)
	// Baris baru & tab dibuang: nama tampil dalam satu baris di panel peserta.
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxGuestNameLen {
		s = strings.TrimSpace(s[:maxGuestNameLen])
	}
	return s
}

// GuestLink menyalakan/mematikan tautan tamu. Hanya host/direksi.
//
// Menyalakan saat sudah menyala akan MEMBUAT ULANG token (rotasi) — itu cara
// mencabut tautan yang terlanjur tersebar tanpa harus mematikan fiturnya.
func (s *Service) GuestLink(u domain.User, id string, enable bool) (domain.Meeting, string, string, error) {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return domain.Meeting{}, "", "", err
	}
	if !m.CanManage(u) {
		return domain.Meeting{}, "", "", domain.ErrForbidden
	}

	token, code := "", ""
	if enable {
		if token, err = newGuestToken(); err != nil {
			return domain.Meeting{}, "", "", err
		}
		if code, err = newGuestCode(); err != nil {
			return domain.Meeting{}, "", "", err
		}
	}
	saved, err := s.st.UpdateMeeting(id, func(mm *domain.Meeting) error {
		mm.GuestEnabled = enable
		// Dimatikan → token DIHAPUS, bukan sekadar ditandai nonaktif. Token yang
		// masih tersimpan adalah tautan yang masih bisa dipakai kalau suatu saat
		// pemeriksaan `GuestEnabled` terlewat di jalur baru.
		mm.GuestToken = token
		mm.GuestCode = code
		return nil
	})
	if err != nil {
		return domain.Meeting{}, "", "", err
	}
	// Rotasi tautan = percobaan yang tercatat ikut dilupakan; kalau tidak,
	// tautan BARU lahir dalam keadaan sudah didinginkan.
	s.lupakanPercobaanTamu(id)
	s.push("meeting.updated", saved)
	return saved, token, code, nil
}

// GuestToken mengembalikan token + kode yang berlaku (untuk menyusun ulang
// tautannya di UI). Hanya host/direksi.
func (s *Service) GuestToken(u domain.User, id string) (string, string, error) {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return "", "", err
	}
	if !m.CanManage(u) {
		return "", "", domain.ErrForbidden
	}
	if !m.GuestEnabled || m.GuestToken == "" {
		return "", "", domain.ErrNotFound
	}
	return m.GuestToken, m.GuestCode, nil
}

/* ---- pembatas percobaan kode ------------------------------------------- */

// percobaanTamu mencatat kegagalan kode per rapat. Di memori dengan sengaja:
// pembatas ini melindungi dari penebakan beruntun dalam hitungan menit, dan
// layanan yang di-restart di tengah serangan tetap menyisakan hambatan utama —
// token 32 byte yang harus dipegang lebih dulu.
type percobaanTamu struct {
	gagal  int
	sampai time.Time // selama masih di depan, tautan didinginkan
}

// bolehCobaKode melaporkan apakah token ini masih boleh mencoba kode.
func (s *Service) bolehCobaKode(meetingID string) (bool, time.Duration) {
	s.guestMu.Lock()
	defer s.guestMu.Unlock()
	p, ok := s.guestTries[meetingID]
	if !ok || p.sampai.IsZero() {
		return true, 0
	}
	if sisa := time.Until(p.sampai); sisa > 0 {
		return false, sisa
	}
	delete(s.guestTries, meetingID)
	return true, 0
}

// catatKodeSalah menambah hitungan gagal dan mendinginkan tautan bila melampaui
// batas.
func (s *Service) catatKodeSalah(meetingID string) {
	s.guestMu.Lock()
	defer s.guestMu.Unlock()
	if s.guestTries == nil {
		s.guestTries = map[string]*percobaanTamu{}
	}
	p, ok := s.guestTries[meetingID]
	if !ok {
		p = &percobaanTamu{}
		s.guestTries[meetingID] = p
	}
	p.gagal++
	if p.gagal >= maxGuestCodeAttempts {
		p.sampai = time.Now().Add(guestCodeCooldown)
		p.gagal = 0
	}
}

// lupakanPercobaanTamu menghapus catatan kegagalan (kode benar / tautan dirotasi).
func (s *Service) lupakanPercobaanTamu(meetingID string) {
	s.guestMu.Lock()
	defer s.guestMu.Unlock()
	delete(s.guestTries, meetingID)
}

// meetingByGuestToken mencari meeting dari token tamu.
func (s *Service) meetingByGuestToken(token string) (domain.Meeting, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return domain.Meeting{}, domain.ErrNotFound
	}
	for _, m := range s.st.ListMeetings(domain.MeetingFilter{}) {
		if m.GuestEnabled && m.GuestToken != "" && m.GuestToken == token {
			return m, nil
		}
	}
	// Token salah dan rapat tidak ada dijawab SAMA — "tidak ditemukan" — supaya
	// tautan yang ditebak tidak bisa dipakai memastikan sebuah rapat itu ada.
	return domain.Meeting{}, domain.ErrNotFound
}

// GuestMeetingInfo adalah keterangan MINIMAL untuk layar persiapan tamu.
// Sengaja tidak memuat daftar peserta, divisi, atau id — orang luar tidak perlu
// tahu susunan internal untuk sekadar bergabung.
type GuestMeetingInfo struct {
	Title  string `json:"title"`
	Status string `json:"status"`
	Kind   string `json:"kind"`
	// CodeRequired memberi tahu halaman tamu untuk meminta kode. Nilai kodenya
	// sendiri TIDAK pernah ikut -- halaman ini publik.
	CodeRequired bool `json:"codeRequired"`
}

// GuestMeeting dipakai halaman tamu sebelum ia mengisi nama. PUBLIK.
func (s *Service) GuestMeeting(token string) (GuestMeetingInfo, error) {
	m, err := s.meetingByGuestToken(token)
	if err != nil {
		return GuestMeetingInfo{}, err
	}
	return GuestMeetingInfo{
		Title: m.Title, Status: string(m.Status), Kind: string(m.Kind),
		CodeRequired: m.GuestCode != "",
	}, nil
}

// guestRole menentukan peran tamu. TIDAK memakai RoleFor: aturan di sana bisa
// mengangkat seseorang jadi host (direksi, pembuat, undangan), dan tamu tidak
// boleh pernah menyentuh jalur itu.
func guestRole(m domain.Meeting) domain.Role {
	if m.Kind == domain.KindStream {
		return domain.RoleViewer
	}
	return domain.RoleSpeaker
}

// JoinAsGuest menerbitkan token LiveKit untuk peserta luar. PUBLIK — tidak ada
// SSO di sini, jadi tokenlah satu-satunya bukti berhak.
func (s *Service) JoinAsGuest(ctx context.Context, token, nama, kode string) (JoinResult, error) {
	m, err := s.meetingByGuestToken(token)
	if err != nil {
		return JoinResult{}, err
	}
	if m.Status == domain.StatusEnded {
		return JoinResult{}, fmt.Errorf("%w: rapat sudah berakhir", domain.ErrValidation)
	}

	// Kode diperiksa SEBELUM apa pun disiapkan: room tidak dibuat, token tidak
	// diterbitkan, tidak ada jejak untuk penebak. Tautan lama tanpa kode
	// (GuestCode kosong) tetap berlaku apa adanya.
	if m.GuestCode != "" {
		if boleh, sisa := s.bolehCobaKode(m.ID); !boleh {
			return JoinResult{}, fmt.Errorf("%w: terlalu banyak percobaan, coba lagi dalam %d menit",
				domain.ErrValidation, int(sisa.Minutes())+1)
		}
		// ConstantTimeCompare: membandingkan dengan == akan berhenti di angka
		// pertama yang beda, dan selisih waktunya bisa dipakai menebak kode
		// digit demi digit.
		diberi := strings.TrimSpace(kode)
		if subtle.ConstantTimeCompare([]byte(diberi), []byte(m.GuestCode)) != 1 {
			s.catatKodeSalah(m.ID)
			return JoinResult{}, fmt.Errorf("%w: kode akses salah", domain.ErrValidation)
		}
		s.lupakanPercobaanTamu(m.ID)
	}

	if !s.lkc.Enabled() {
		return JoinResult{}, domain.ErrDisabled
	}

	nama = bersihkanNamaTamu(nama)
	if nama == "" {
		return JoinResult{}, fmt.Errorf("%w: nama wajib diisi", domain.ErrValidation)
	}

	if _, err := s.lkc.CreateRoom(ctx, lk.RoomOptions{
		Name:            m.Room,
		Metadata:        meetingMetadata(m),
		EmptyTimeout:    m.EmptyTimeout,
		MaxParticipants: m.MaxParticipants,
	}); err != nil {
		// Yang membaca pesan ini orang LUAR. Galat asli membawa alamat dan nama
		// layanan internal ("http://localhost:7880/twirp/livekit.RoomService/…"),
		// dan itu tidak boleh keluar dari jalur yang tidak terautentikasi — juga
		// tidak berguna baginya. Rinciannya dicatat di log; tamunya diberi
		// kalimat yang bisa ia tindak lanjuti.
		log.Printf("livekit: gagal menyiapkan room tamu %q: %v", m.Room, err)
		return JoinResult{}, fmt.Errorf("%w: rapat belum bisa dimulai — hubungi pengundang Anda",
			domain.ErrValidation)
	}

	// Identitas tamu diberi awalan "tamu:" DAN akhiran acak. Awalannya supaya
	// peserta internal langsung terlihat bedanya di panel peserta; akhirannya
	// supaya dua tamu bernama sama tidak saling menendang keluar — LiveKit
	// memutus koneksi lama bila identitasnya bentrok.
	sid, err := newGuestToken()
	if err != nil {
		return JoinResult{}, err
	}
	role := guestRole(m)
	tok, exp, err := s.lkc.Token(lk.Grant{
		Room:                 m.Room,
		Identity:             "tamu:" + sid[:12],
		Name:                 nama + " (tamu)",
		CanPublish:           role.CanPublish(),
		CanSubscribe:         true,
		CanPublishData:       true,
		CanUpdateOwnMetadata: true,
		// Tiga hal berikut sengaja dibiarkan false — inilah yang memisahkan tamu
		// dari peserta internal, dan satu-satunya penjaga saat permintaan datang
		// dari orang yang tidak kita kenal sama sekali.
		RoomAdmin:  false,
		RoomCreate: false,
		RoomList:   false,
	})
	if err != nil {
		return JoinResult{}, err
	}

	if m.Status != domain.StatusLive {
		if updated, uerr := s.st.UpdateMeeting(m.ID, func(mm *domain.Meeting) error {
			mm.Status = domain.StatusLive
			if mm.StartedAt == nil {
				now := time.Now()
				mm.StartedAt = &now
			}
			return nil
		}); uerr == nil {
			m = updated
			s.push("meeting.updated", m)
		}
	}
	s.logEvent(m.Room, "guest.join", nama)

	return JoinResult{
		URL:       s.clientURL(),
		Token:     tok,
		Room:      m.Room,
		Role:      role,
		ExpiresAt: exp,
	}, nil
}
