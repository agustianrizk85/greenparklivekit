// Package store menyimpan state layanan LiveKit (meeting + jejak event webhook)
// ke satu berkas JSON. Tanpa database: seluruh state dimuat ke memori saat New,
// setiap mutasi ditulis ulang secara atomik (tmp + rename). Semua method aman
// dipanggil bersamaan dan method baca selalu mengembalikan salinan.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"greenpark/livekit/internal/domain"
)

// maxEvents adalah kapasitas ring buffer jejak webhook.
const maxEvents = 500

// defaultEventLimit dipakai saat pemanggil ListEvents tidak memberi limit.
const defaultEventLimit = 100

// maxRoomLen membatasi panjang slug room agar aman dipakai sebagai nama room.
const maxRoomLen = 48

// state adalah bentuk berkas JSON di disk.
type state struct {
	Meetings  []domain.Meeting `json:"meetings"`
	Events    []domain.Event   `json:"events"`
	UpdatedAt time.Time        `json:"updatedAt"`
}

// Store adalah persistensi meeting + event berbasis berkas JSON.
type Store struct {
	mu       sync.RWMutex
	path     string
	meetings []domain.Meeting
	events   []domain.Event // urut lama -> baru
}

// New memuat store dari path. Folder induk dibuat otomatis; berkas yang belum
// ada dianggap state kosong (bukan error).
func New(path string) (*Store, error) {
	s := &Store{path: path, meetings: []domain.Meeting{}, events: []domain.Event{}}
	if path == "" {
		return s, nil
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return s, nil
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Meetings != nil {
		s.meetings = st.Meetings
	}
	if st.Events != nil {
		s.events = st.Events
	}
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
	return s, nil
}

// save menulis state ke disk secara atomik. Pemanggil harus memegang lock tulis.
func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(state{
		Meetings:  s.meetings,
		Events:    s.events,
		UpdatedAt: time.Now(),
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---------- Meeting ----------

// ListMeetings mengembalikan salinan meeting yang lolos filter, diurut dari yang
// paling aktif (live > scheduled > ended) lalu waktu terbaru.
func (s *Store) ListMeetings(f domain.MeetingFilter) []domain.Meeting {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.Meeting, 0, len(s.meetings))
	for _, m := range s.meetings {
		if matchFilter(m, f) {
			out = append(out, cloneMeeting(m))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := statusRank(out[i].Status), statusRank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		ti, tj := meetingTime(out[i]), meetingTime(out[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// GetMeeting mencari meeting berdasarkan ID.
func (s *Store) GetMeeting(id string) (domain.Meeting, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := s.indexByID(id); i >= 0 {
		return cloneMeeting(s.meetings[i]), nil
	}
	return domain.Meeting{}, domain.ErrNotFound
}

// GetMeetingByRoom mencari meeting berdasarkan nama room (case-insensitive).
// Meeting yang masih aktif diprioritaskan jika nama room pernah dipakai ulang.
func (s *Store) GetMeetingByRoom(room string) (domain.Meeting, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if i := s.indexByRoom(room); i >= 0 {
		return cloneMeeting(s.meetings[i]), nil
	}
	return domain.Meeting{}, domain.ErrNotFound
}

// CreateMeeting menyimpan meeting baru. ID/CreatedAt/UpdatedAt diisi bila kosong.
// Mengembalikan domain.ErrValidation kalau Room/Title kosong, dan
// domain.ErrConflict kalau room masih dipakai meeting yang belum berakhir.
func (s *Store) CreateMeeting(m domain.Meeting) (domain.Meeting, error) {
	m.Room = strings.TrimSpace(m.Room)
	m.Title = strings.TrimSpace(m.Title)
	if m.Room == "" || m.Title == "" {
		return domain.Meeting{}, domain.ErrValidation
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, ex := range s.meetings {
		if ex.Status != domain.StatusEnded && strings.EqualFold(ex.Room, m.Room) {
			return domain.Meeting{}, domain.ErrConflict
		}
	}
	if m.ID == "" {
		m.ID = NewID()
	} else if s.indexByID(m.ID) >= 0 {
		return domain.Meeting{}, domain.ErrConflict
	}
	now := time.Now()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = now
	}
	if m.Status == "" {
		m.Status = domain.StatusScheduled
	}
	if m.Kind == "" {
		m.Kind = domain.KindMeeting
	}

	s.meetings = append(s.meetings, cloneMeeting(m))
	if err := s.save(); err != nil {
		return domain.Meeting{}, err
	}
	return cloneMeeting(m), nil
}

// UpdateMeeting menjalankan mutate atas SALINAN meeting di bawah lock. Perubahan
// hanya disimpan kalau mutate tidak mengembalikan error; UpdatedAt diperbarui.
func (s *Store) UpdateMeeting(id string, mutate func(*domain.Meeting) error) (domain.Meeting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateAt(s.indexByID(id), mutate)
}

// UpdateMeetingByRoom sama seperti UpdateMeeting tapi mencari lewat nama room.
func (s *Store) UpdateMeetingByRoom(room string, mutate func(*domain.Meeting) error) (domain.Meeting, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateAt(s.indexByRoom(room), mutate)
}

// updateAt adalah inti update; pemanggil harus memegang lock tulis.
func (s *Store) updateAt(i int, mutate func(*domain.Meeting) error) (domain.Meeting, error) {
	if i < 0 {
		return domain.Meeting{}, domain.ErrNotFound
	}
	draft := cloneMeeting(s.meetings[i])
	if mutate != nil {
		if err := mutate(&draft); err != nil {
			return domain.Meeting{}, err
		}
	}
	draft.ID = s.meetings[i].ID // ID tidak boleh diubah lewat mutate
	draft.UpdatedAt = time.Now()

	s.meetings[i] = cloneMeeting(draft)
	if err := s.save(); err != nil {
		return domain.Meeting{}, err
	}
	return cloneMeeting(draft), nil
}

// DeleteMeeting menghapus meeting berdasarkan ID.
func (s *Store) DeleteMeeting(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.indexByID(id)
	if i < 0 {
		return domain.ErrNotFound
	}
	s.meetings = append(s.meetings[:i], s.meetings[i+1:]...)
	return s.save()
}

// indexByID mengembalikan posisi meeting atau -1. Butuh lock (baca/tulis).
func (s *Store) indexByID(id string) int {
	if id == "" {
		return -1
	}
	for i := range s.meetings {
		if s.meetings[i].ID == id {
			return i
		}
	}
	return -1
}

// indexByRoom mencocokkan nama room case-insensitive; yang belum ended menang.
func (s *Store) indexByRoom(room string) int {
	room = strings.TrimSpace(room)
	if room == "" {
		return -1
	}
	found := -1
	for i := range s.meetings {
		if !strings.EqualFold(s.meetings[i].Room, room) {
			continue
		}
		if s.meetings[i].Status != domain.StatusEnded {
			return i
		}
		if found < 0 {
			found = i
		}
	}
	return found
}

// ---------- Event ----------

// AppendEvent menambah jejak webhook ke ring buffer (maksimal 500 terbaru).
// ID dan At diisi otomatis bila kosong.
func (s *Store) AppendEvent(e domain.Event) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	if len(s.events) > maxEvents {
		s.events = append([]domain.Event(nil), s.events[len(s.events)-maxEvents:]...)
	}
	return s.save()
}

// ListEvents mengembalikan event terbaru lebih dulu. room kosong = semua room
// (cocok case-insensitive); limit <= 0 dianggap 100.
func (s *Store) ListEvents(room string, limit int) []domain.Event {
	if limit <= 0 {
		limit = defaultEventLimit
	}
	room = strings.TrimSpace(room)

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.Event, 0, limit)
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		e := s.events[i]
		if room != "" && !strings.EqualFold(e.Room, room) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ---------- Ringkasan ----------

// Stats mengembalikan hitungan ringkas isi store.
func (s *Store) Stats() map[string]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{
		"meetings":  len(s.meetings),
		"live":      0,
		"scheduled": 0,
		"ended":     0,
		"events":    len(s.events),
	}
	for _, m := range s.meetings {
		switch m.Status {
		case domain.StatusLive:
			out["live"]++
		case domain.StatusScheduled:
			out["scheduled"]++
		case domain.StatusEnded:
			out["ended"]++
		}
	}
	return out
}

// ---------- Utilitas ----------

// NewID mengembalikan ID acak pendek (10 byte crypto/rand, hex 20 karakter).
func NewID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		// Fallback berbasis waktu kalau entropi sistem gagal.
		return "id" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// SlugRoom mengubah judul menjadi nama room yang aman: huruf kecil, hanya
// [a-z0-9-], maksimal 48 karakter. Judul tanpa karakter valid -> "room-<id>".
func SlugRoom(title string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(title)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > maxRoomLen {
		slug = strings.Trim(slug[:maxRoomLen], "-")
	}
	if slug == "" {
		return "room-" + NewID()
	}
	return slug
}

// suffixLen adalah panjang akhiran acak pada SlugRoomUnique. Enam karakter hex
// = 16 juta kemungkinan; cukup jauh untuk room yang umurnya hitungan jam.
const suffixLen = 6

// SlugRoomUnique sama seperti SlugRoom, tapi selalu menambahkan akhiran acak
// sehingga judul yang sama TIDAK pernah menghasilkan nama room yang sama.
//
// Dipakai untuk room yang namanya diturunkan otomatis dari judul — terutama
// panggilan, yang judulnya berisi nama peserta ("Panggilan grup — Budi, Ani").
// Dengan SlugRoom biasa, menelepon orang yang sama dua kali menghasilkan nama
// room yang identik; kalau panggilan sebelumnya belum berstatus ended (peserta
// menutup tab, jaringan putus, atau panggilannya gagal di tengah), CreateMeeting
// menolaknya sebagai ErrConflict dan pemakai melihat pesan "data sudah ada"
// padahal ia merasa tidak sedang menelepon siapa pun.
//
// Room yang namanya DISEBUT EKSPLISIT sengaja tidak lewat sini: untuk rapat
// terjadwal (mis. "rapat-mingguan") nama yang tetap justru yang diinginkan, dan
// pemeriksaan bentrokan di sana memang benar.
func SlugRoomUnique(title string) string {
	slug := SlugRoom(title)
	// Sisakan tempat untuk "-" + akhiran supaya tetap di bawah maxRoomLen.
	if len(slug)+1+suffixLen > maxRoomLen {
		slug = strings.Trim(slug[:maxRoomLen-suffixLen-1], "-")
	}
	if slug == "" {
		return "room-" + shortID()
	}
	return slug + "-" + shortID()
}

// shortID mengembalikan akhiran acak sepanjang suffixLen karakter hex.
func shortID() string {
	b := make([]byte, (suffixLen+1)/2)
	if _, err := rand.Read(b); err != nil {
		// Entropi sistem gagal — jam sistem sudah cukup untuk membedakan, karena
		// yang dicegah di sini cuma tabrakan antar-panggilan berturut-turut.
		s := strconv.FormatInt(time.Now().UnixNano(), 36)
		if len(s) > suffixLen {
			return s[len(s)-suffixLen:]
		}
		return s
	}
	return hex.EncodeToString(b)[:suffixLen]
}

// matchFilter melaporkan apakah meeting lolos seluruh kriteria filter.
func matchFilter(m domain.Meeting, f domain.MeetingFilter) bool {
	if d := strings.TrimSpace(f.Division); d != "" {
		if !strings.EqualFold(m.Division, d) && !containsFold(m.Divisions, d) {
			return false
		}
	}
	if f.Status != "" && m.Status != f.Status {
		return false
	}
	if f.Kind != "" && m.Kind != f.Kind {
		return false
	}
	if r := strings.TrimSpace(f.Room); r != "" && !strings.EqualFold(m.Room, r) {
		return false
	}
	if q := strings.ToLower(strings.TrimSpace(f.Query)); q != "" {
		hay := strings.ToLower(strings.Join([]string{m.Title, m.Description, m.Room, m.HostName}, "\x00"))
		if !strings.Contains(hay, q) {
			return false
		}
	}
	return true
}

// containsFold melaporkan apakah slice memuat v (case-insensitive).
func containsFold(xs []string, v string) bool {
	for _, x := range xs {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// statusRank memberi bobot urutan: live paling atas, ended paling bawah.
func statusRank(st domain.Status) int {
	switch st {
	case domain.StatusLive:
		return 0
	case domain.StatusScheduled:
		return 1
	case domain.StatusEnded:
		return 3
	default:
		return 2
	}
}

// meetingTime memilih stempel waktu paling relevan untuk pengurutan.
func meetingTime(m domain.Meeting) time.Time {
	switch m.Status {
	case domain.StatusLive:
		if m.StartedAt != nil {
			return *m.StartedAt
		}
	case domain.StatusScheduled:
		if m.ScheduledAt != nil {
			return *m.ScheduledAt
		}
	case domain.StatusEnded:
		if m.EndedAt != nil {
			return *m.EndedAt
		}
	}
	if !m.UpdatedAt.IsZero() {
		return m.UpdatedAt
	}
	return m.CreatedAt
}

// cloneMeeting menyalin meeting beserta seluruh slice dan pointer waktunya,
// supaya state internal tidak bisa dirusak lewat referensi bersama.
func cloneMeeting(m domain.Meeting) domain.Meeting {
	out := m
	out.Divisions = cloneStrings(m.Divisions)
	out.ScheduledAt = cloneTime(m.ScheduledAt)
	out.StartedAt = cloneTime(m.StartedAt)
	out.EndedAt = cloneTime(m.EndedAt)
	if m.Invitees != nil {
		out.Invitees = append([]domain.Invitee(nil), m.Invitees...)
	}
	if m.Recordings != nil {
		out.Recordings = make([]domain.Recording, len(m.Recordings))
		for i, r := range m.Recordings {
			r.URLs = cloneStrings(r.URLs)
			r.EndedAt = cloneTime(r.EndedAt)
			out.Recordings[i] = r
		}
	}
	return out
}

// cloneStrings menyalin slice string (nil tetap nil).
func cloneStrings(xs []string) []string {
	if xs == nil {
		return nil
	}
	return append([]string(nil), xs...)
}

// cloneTime menyalin pointer waktu (nil tetap nil).
func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}
