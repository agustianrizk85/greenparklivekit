package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
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
func (s *Service) GuestLink(u domain.User, id string, enable bool) (domain.Meeting, string, error) {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return domain.Meeting{}, "", err
	}
	if !m.CanManage(u) {
		return domain.Meeting{}, "", domain.ErrForbidden
	}

	token := ""
	if enable {
		if token, err = newGuestToken(); err != nil {
			return domain.Meeting{}, "", err
		}
	}
	saved, err := s.st.UpdateMeeting(id, func(mm *domain.Meeting) error {
		mm.GuestEnabled = enable
		// Dimatikan → token DIHAPUS, bukan sekadar ditandai nonaktif. Token yang
		// masih tersimpan adalah tautan yang masih bisa dipakai kalau suatu saat
		// pemeriksaan `GuestEnabled` terlewat di jalur baru.
		mm.GuestToken = token
		return nil
	})
	if err != nil {
		return domain.Meeting{}, "", err
	}
	s.push("meeting.updated", saved)
	return saved, token, nil
}

// GuestToken mengembalikan token yang berlaku (untuk menyusun ulang tautannya
// di UI). Hanya host/direksi.
func (s *Service) GuestToken(u domain.User, id string) (string, error) {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return "", err
	}
	if !m.CanManage(u) {
		return "", domain.ErrForbidden
	}
	if !m.GuestEnabled || m.GuestToken == "" {
		return "", domain.ErrNotFound
	}
	return m.GuestToken, nil
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
}

// GuestMeeting dipakai halaman tamu sebelum ia mengisi nama. PUBLIK.
func (s *Service) GuestMeeting(token string) (GuestMeetingInfo, error) {
	m, err := s.meetingByGuestToken(token)
	if err != nil {
		return GuestMeetingInfo{}, err
	}
	return GuestMeetingInfo{Title: m.Title, Status: string(m.Status), Kind: string(m.Kind)}, nil
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
func (s *Service) JoinAsGuest(ctx context.Context, token, nama string) (JoinResult, error) {
	m, err := s.meetingByGuestToken(token)
	if err != nil {
		return JoinResult{}, err
	}
	if m.Status == domain.StatusEnded {
		return JoinResult{}, fmt.Errorf("%w: rapat sudah berakhir", domain.ErrValidation)
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
		return JoinResult{}, fmt.Errorf("gagal menyiapkan room: %w", err)
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
