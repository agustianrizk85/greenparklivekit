// Package service berisi aturan bisnis layanan LiveKit Greenpark: siapa boleh
// masuk room mana, penerbitan access token, kendali room (kick/mute/akhiri),
// egress (rekam & streaming), dispatch voice AI agent, serta pemutakhiran
// status meeting dari webhook LiveKit.
package service

import (
	"context"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"path"
	"strings"
	"sync"
	"time"

	"greenpark/livekit/internal/domain"
	"greenpark/livekit/internal/lk"
	"greenpark/livekit/internal/store"
)

// Broadcaster mengirim perubahan ke klien realtime (WebSocket). Transport yang
// menyediakan implementasinya; service hanya memanggil.
type Broadcaster interface {
	Broadcast(kind string, payload any)
}

// Options adalah konfigurasi service yang berasal dari config paket.
type Options struct {
	RecordDir string
	AgentName string
	TokenTTL  time.Duration
	// PublicURL adalah alamat LiveKit yang dikirim ke browser. Kosong = pakai
	// alamat yang dipakai service ini sendiri. Keduanya berbeda saat LiveKit
	// di-self-host: service memanggil ws://127.0.0.1:7880, sedangkan halaman
	// https hanya boleh menyambung ke wss:// lewat reverse proxy.
	PublicURL string
}

// Service menyatukan store, klien LiveKit, dan kebijakan akses.
type Service struct {
	st   *store.Store
	lkc  *lk.Client
	opts Options
	bc   Broadcaster

	// Pembatas percobaan kode tamu, per rapat (lihat guest.go).
	guestMu    sync.Mutex
	guestTries map[string]*percobaanTamu
}

// New membuat Service.
func New(st *store.Store, lkc *lk.Client, opts Options) *Service {
	return &Service{st: st, lkc: lkc, opts: opts}
}

// SetBroadcaster memasang kanal realtime (opsional).
func (s *Service) SetBroadcaster(b Broadcaster) { s.bc = b }

func (s *Service) push(kind string, payload any) {
	if s.bc != nil {
		s.bc.Broadcast(kind, payload)
	}
}

// Config adalah info yang dibutuhkan frontend untuk menyambung ke LiveKit.
type Config struct {
	URL       string `json:"url"`
	Enabled   bool   `json:"enabled"`
	Egress    bool   `json:"egress"`
	AgentName string `json:"agentName,omitempty"`
	TokenTTL  int    `json:"tokenTtlMinutes"`
}

// clientURL adalah alamat LiveKit yang layak dikirim ke browser.
func (s *Service) clientURL() string {
	if u := strings.TrimSpace(s.opts.PublicURL); u != "" {
		return u
	}
	return s.lkc.URL()
}

// Config mengembalikan konfigurasi klien.
func (s *Service) Config() Config {
	return Config{
		URL:       s.clientURL(),
		Enabled:   s.lkc.Enabled(),
		Egress:    s.lkc.Enabled(),
		AgentName: s.opts.AgentName,
		TokenTTL:  int(s.opts.TokenTTL / time.Minute),
	}
}

// ---------- meeting CRUD ----------

// MeetingInput adalah payload pembuatan/perubahan meeting.
type MeetingInput struct {
	Room            string            `json:"room"`
	Title           string            `json:"title"`
	Description     string            `json:"description"`
	Kind            domain.Kind       `json:"kind"`
	Division        string            `json:"division"`
	Divisions       []string          `json:"divisions"`
	Open            *bool             `json:"open"`
	ScheduledAt     *time.Time        `json:"scheduledAt"`
	MaxParticipants uint32            `json:"maxParticipants"`
	EmptyTimeout    uint32            `json:"emptyTimeout"`
	RecordEnabled   *bool             `json:"recordEnabled"`
	AgentEnabled    *bool             `json:"agentEnabled"`
	AgentName       string            `json:"agentName"`
	AgentMetadata   string            `json:"agentMetadata"`
	Invitees        []domain.Invitee  `json:"invitees"`
	Status          domain.Status     `json:"status"`
	Extra           map[string]string `json:"-"`
}

// ListMeetings mengembalikan meeting yang boleh dilihat user.
func (s *Service) ListMeetings(u domain.User, f domain.MeetingFilter) []domain.Meeting {
	all := s.st.ListMeetings(f)
	out := make([]domain.Meeting, 0, len(all))
	for _, m := range all {
		if m.Visible(u) {
			out = append(out, m)
		}
	}
	return out
}

// GetMeeting mengembalikan satu meeting bila user berhak melihatnya.
func (s *Service) GetMeeting(u domain.User, id string) (domain.Meeting, error) {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return domain.Meeting{}, err
	}
	if !m.Visible(u) {
		return domain.Meeting{}, domain.ErrForbidden
	}
	return m, nil
}

// CreateMeeting membuat meeting baru; pembuat otomatis jadi host.
func (s *Service) CreateMeeting(ctx context.Context, u domain.User, in MeetingInput) (domain.Meeting, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return domain.Meeting{}, fmt.Errorf("%w: judul wajib diisi", domain.ErrValidation)
	}
	// Room yang disebut eksplisit tetap deterministik (bentrokan di situ memang
	// harus dicegah). Yang diturunkan dari judul diberi akhiran acak — lihat
	// store.SlugRoomUnique untuk alasannya ("data sudah ada" pada panggilan
	// berulang ke orang yang sama).
	room := store.SlugRoom(strings.TrimSpace(in.Room))
	if strings.TrimSpace(in.Room) == "" {
		room = store.SlugRoomUnique(title)
	}
	kind := in.Kind
	if kind == "" {
		kind = domain.KindMeeting
	}
	division := strings.TrimSpace(in.Division)
	if division == "" {
		if ds := u.Divisions(); len(ds) > 0 {
			division = ds[0]
		}
	}
	now := time.Now()
	m := domain.Meeting{
		Room:            room,
		Title:           title,
		Description:     strings.TrimSpace(in.Description),
		Kind:            kind,
		Status:          domain.StatusScheduled,
		Division:        division,
		Divisions:       in.Divisions,
		Open:            in.Open != nil && *in.Open,
		HostID:          u.ID,
		HostName:        firstNonEmpty(u.Name, u.Username),
		ScheduledAt:     in.ScheduledAt,
		MaxParticipants: in.MaxParticipants,
		EmptyTimeout:    in.EmptyTimeout,
		RecordEnabled:   in.RecordEnabled != nil && *in.RecordEnabled,
		AgentEnabled:    in.AgentEnabled != nil && *in.AgentEnabled,
		AgentName:       firstNonEmpty(strings.TrimSpace(in.AgentName), s.opts.AgentName),
		AgentMetadata:   in.AgentMetadata,
		Invitees:        normalizeInvitees(in.Invitees),
		CreatedBy:       firstNonEmpty(u.Username, u.ID),
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if in.ScheduledAt == nil {
		m.Status = domain.StatusScheduled
	}
	saved, err := s.st.CreateMeeting(m)
	if err != nil {
		return domain.Meeting{}, err
	}
	s.push("meeting.created", saved)
	return saved, nil
}

// UpdateMeeting mengubah meeting (hanya host/direktur).
func (s *Service) UpdateMeeting(u domain.User, id string, in MeetingInput) (domain.Meeting, error) {
	cur, err := s.st.GetMeeting(id)
	if err != nil {
		return domain.Meeting{}, err
	}
	if !cur.CanManage(u) {
		return domain.Meeting{}, domain.ErrForbidden
	}
	saved, err := s.st.UpdateMeeting(id, func(m *domain.Meeting) error {
		if t := strings.TrimSpace(in.Title); t != "" {
			m.Title = t
		}
		if in.Description != "" {
			m.Description = strings.TrimSpace(in.Description)
		}
		if in.Kind != "" {
			m.Kind = in.Kind
		}
		if in.Division != "" {
			m.Division = in.Division
		}
		if in.Divisions != nil {
			m.Divisions = in.Divisions
		}
		if in.Open != nil {
			m.Open = *in.Open
		}
		if in.ScheduledAt != nil {
			m.ScheduledAt = in.ScheduledAt
		}
		if in.MaxParticipants > 0 {
			m.MaxParticipants = in.MaxParticipants
		}
		if in.EmptyTimeout > 0 {
			m.EmptyTimeout = in.EmptyTimeout
		}
		if in.RecordEnabled != nil {
			m.RecordEnabled = *in.RecordEnabled
		}
		if in.AgentEnabled != nil {
			m.AgentEnabled = *in.AgentEnabled
		}
		if in.AgentName != "" {
			m.AgentName = strings.TrimSpace(in.AgentName)
		}
		if in.AgentMetadata != "" {
			m.AgentMetadata = in.AgentMetadata
		}
		if in.Invitees != nil {
			m.Invitees = normalizeInvitees(in.Invitees)
		}
		if in.Status != "" {
			m.Status = in.Status
		}
		return nil
	})
	if err != nil {
		return domain.Meeting{}, err
	}
	s.push("meeting.updated", saved)
	return saved, nil
}

// DeleteMeeting menghapus meeting dan menutup room-nya bila masih hidup.
func (s *Service) DeleteMeeting(ctx context.Context, u domain.User, id string) error {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return err
	}
	if !m.CanManage(u) {
		return domain.ErrForbidden
	}
	if s.lkc.Enabled() && m.Status == domain.StatusLive {
		_ = s.lkc.DeleteRoom(ctx, m.Room)
	}
	if err := s.st.DeleteMeeting(id); err != nil {
		return err
	}
	s.push("meeting.deleted", map[string]string{"id": id, "room": m.Room})
	return nil
}

// ---------- token & join ----------

// JoinResult adalah jawaban untuk klien yang hendak masuk room.
type JoinResult struct {
	URL       string          `json:"url"`
	Token     string          `json:"token"`
	Room      string          `json:"room"`
	Role      domain.Role     `json:"role"`
	Identity  string          `json:"identity"`
	ExpiresAt time.Time       `json:"expiresAt"`
	Meeting   *domain.Meeting `json:"meeting,omitempty"`
}

// Join menerbitkan access token untuk meeting tertentu dan menandai meeting
// sebagai live bila ini peserta pertama.
func (s *Service) Join(ctx context.Context, u domain.User, id string) (JoinResult, error) {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return JoinResult{}, err
	}
	role, ok := m.RoleFor(u)
	if !ok {
		return JoinResult{}, domain.ErrForbidden
	}
	if m.Status == domain.StatusEnded {
		return JoinResult{}, fmt.Errorf("%w: meeting sudah berakhir", domain.ErrValidation)
	}
	if !s.lkc.Enabled() {
		return JoinResult{}, domain.ErrDisabled
	}

	// Pastikan room ada dengan batasan sesuai meeting (idempoten).
	if _, err := s.lkc.CreateRoom(ctx, lk.RoomOptions{
		Name:            m.Room,
		Metadata:        meetingMetadata(m),
		EmptyTimeout:    m.EmptyTimeout,
		MaxParticipants: m.MaxParticipants,
	}); err != nil {
		return JoinResult{}, fmt.Errorf("gagal menyiapkan room: %w", err)
	}

	tok, exp, err := s.lkc.Token(s.grantFor(m, u, role))
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

	// Dispatch voice AI agent bila diminta (tidak fatal kalau gagal).
	if m.AgentEnabled {
		if name := firstNonEmpty(m.AgentName, s.opts.AgentName); name != "" {
			if _, derr := s.lkc.DispatchAgent(ctx, lk.DispatchOptions{Room: m.Room, AgentName: name, Metadata: m.AgentMetadata}); derr != nil {
				s.logEvent(m.Room, "agent.dispatch.failed", derr.Error())
			}
		}
	}

	return JoinResult{
		URL:       s.clientURL(),
		Token:     tok,
		Room:      m.Room,
		Role:      role,
		Identity:  identityOf(u),
		ExpiresAt: exp,
		Meeting:   &m,
	}, nil
}

// TokenRequest adalah permintaan token ad-hoc (tanpa entri meeting).
type TokenRequest struct {
	Room     string      `json:"room"`
	Role     domain.Role `json:"role"`
	Identity string      `json:"identity"`
	Name     string      `json:"name"`
	TTLMin   int         `json:"ttlMinutes"`
}

// AdHocToken menerbitkan token untuk room bebas. Kalau room-nya terdaftar
// sebagai meeting, kebijakan meeting tetap berlaku.
func (s *Service) AdHocToken(ctx context.Context, u domain.User, req TokenRequest) (JoinResult, error) {
	room := store.SlugRoom(strings.TrimSpace(req.Room))
	if room == "" {
		return JoinResult{}, fmt.Errorf("%w: nama room wajib diisi", domain.ErrValidation)
	}
	if !s.lkc.Enabled() {
		return JoinResult{}, domain.ErrDisabled
	}
	if m, err := s.st.GetMeetingByRoom(room); err == nil {
		return s.Join(ctx, u, m.ID)
	}

	role := req.Role
	if !role.Valid() {
		role = domain.RoleSpeaker
	}
	// Room bebas hanya boleh dibuat oleh direktur/super; sisanya viewer saja.
	if role.IsAdmin() && !u.IsDirector() {
		role = domain.RoleSpeaker
	}
	g := lk.Grant{
		Room:                 room,
		Identity:             firstNonEmpty(strings.TrimSpace(req.Identity), identityOf(u)),
		Name:                 firstNonEmpty(strings.TrimSpace(req.Name), u.Name, u.Username),
		Metadata:             userMetadata(u, role),
		CanPublish:           role.CanPublish(),
		CanSubscribe:         true,
		CanPublishData:       true,
		CanUpdateOwnMetadata: true,
		RoomAdmin:            role.IsAdmin() || u.IsDirector(),
		RoomCreate:           u.IsDirector(),
		Sources:              sourcesFor(role),
	}
	if req.TTLMin > 0 {
		g.TTL = time.Duration(req.TTLMin) * time.Minute
	}
	tok, exp, err := s.lkc.Token(g)
	if err != nil {
		return JoinResult{}, err
	}
	return JoinResult{URL: s.clientURL(), Token: tok, Room: room, Role: role, Identity: g.Identity, ExpiresAt: exp}, nil
}

func (s *Service) grantFor(m domain.Meeting, u domain.User, role domain.Role) lk.Grant {
	return lk.Grant{
		Room:                 m.Room,
		Identity:             identityOf(u),
		Name:                 firstNonEmpty(u.Name, u.Username),
		Metadata:             userMetadata(u, role),
		CanPublish:           role.CanPublish(),
		CanSubscribe:         true,
		CanPublishData:       true,
		CanUpdateOwnMetadata: true,
		RoomAdmin:            role.IsAdmin(),
		Sources:              sourcesFor(role),
		Attributes:           map[string]string{"divisi": m.Division, "peran": string(role)},
	}
}

// ---------- kendali room ----------

// Participants mengembalikan peserta yang sedang berada di room meeting.
func (s *Service) Participants(ctx context.Context, u domain.User, id string) ([]lk.ParticipantInfo, error) {
	m, err := s.GetMeeting(u, id)
	if err != nil {
		return nil, err
	}
	return s.lkc.ListParticipants(ctx, m.Room)
}

// Kick mengeluarkan peserta dari room (host/direktur).
func (s *Service) Kick(ctx context.Context, u domain.User, id, identity string) error {
	m, err := s.manageable(u, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(identity) == "" {
		return fmt.Errorf("%w: identity peserta wajib diisi", domain.ErrValidation)
	}
	return s.lkc.RemoveParticipant(ctx, m.Room, identity)
}

// Mute membisukan/mengaktifkan satu track peserta (host/direktur).
func (s *Service) Mute(ctx context.Context, u domain.User, id, identity, trackSID string, muted bool) error {
	m, err := s.manageable(u, id)
	if err != nil {
		return err
	}
	if identity == "" || trackSID == "" {
		return fmt.Errorf("%w: identity dan trackSid wajib diisi", domain.ErrValidation)
	}
	return s.lkc.MuteTrack(ctx, m.Room, identity, trackSID, muted)
}

// Start menandai meeting dimulai dan menyiapkan room-nya.
func (s *Service) Start(ctx context.Context, u domain.User, id string) (domain.Meeting, error) {
	m, err := s.manageable(u, id)
	if err != nil {
		return domain.Meeting{}, err
	}
	if s.lkc.Enabled() {
		if _, err := s.lkc.CreateRoom(ctx, lk.RoomOptions{
			Name: m.Room, Metadata: meetingMetadata(m),
			EmptyTimeout: m.EmptyTimeout, MaxParticipants: m.MaxParticipants,
		}); err != nil {
			return domain.Meeting{}, err
		}
	}
	saved, err := s.st.UpdateMeeting(id, func(mm *domain.Meeting) error {
		mm.Status = domain.StatusLive
		if mm.StartedAt == nil {
			now := time.Now()
			mm.StartedAt = &now
		}
		mm.EndedAt = nil
		return nil
	})
	if err != nil {
		return domain.Meeting{}, err
	}
	s.push("meeting.updated", saved)
	return saved, nil
}

// End menutup room dan menandai meeting selesai.
func (s *Service) End(ctx context.Context, u domain.User, id string) (domain.Meeting, error) {
	m, err := s.manageable(u, id)
	if err != nil {
		return domain.Meeting{}, err
	}
	if s.lkc.Enabled() {
		// Hentikan egress yang masih jalan, lalu tutup room.
		if list, lerr := s.lkc.ListEgress(ctx, m.Room, true); lerr == nil {
			for _, e := range list {
				_, _ = s.lkc.StopEgress(ctx, e.EgressID)
			}
		}
		_ = s.lkc.DeleteRoom(ctx, m.Room)
	}
	saved, err := s.st.UpdateMeeting(id, func(mm *domain.Meeting) error {
		mm.Status = domain.StatusEnded
		now := time.Now()
		mm.EndedAt = &now
		mm.NumParticipants = 0
		return nil
	})
	if err != nil {
		return domain.Meeting{}, err
	}
	s.push("meeting.updated", saved)
	return saved, nil
}

// ---------- egress: rekam & streaming ----------

// StartRecord memulai perekaman komposit room ke berkas MP4.
func (s *Service) StartRecord(ctx context.Context, u domain.User, id, layout string) (lk.EgressInfo, error) {
	m, err := s.manageable(u, id)
	if err != nil {
		return lk.EgressInfo{}, err
	}
	filepath := path.Join(s.opts.RecordDir, fmt.Sprintf("%s-%s.mp4", m.Room, time.Now().Format("20060102-150405")))
	info, err := s.lkc.StartRecord(ctx, lk.RecordOptions{Room: m.Room, Layout: layout, Filepath: filepath})
	if err != nil {
		return lk.EgressInfo{}, err
	}
	s.attachRecording(m.ID, domain.Recording{
		EgressID: info.EgressID, Kind: "record", Status: info.Status, Layout: layout,
		Filepath: filepath, StartedAt: time.Now(), StartedBy: firstNonEmpty(u.Username, u.ID),
	})
	return info, nil
}

// StartStream memulai siaran RTMP (mis. YouTube/Facebook Live) dari room.
func (s *Service) StartStream(ctx context.Context, u domain.User, id, layout string, urls []string) (lk.EgressInfo, error) {
	m, err := s.manageable(u, id)
	if err != nil {
		return lk.EgressInfo{}, err
	}
	clean := make([]string, 0, len(urls))
	for _, raw := range urls {
		if v := strings.TrimSpace(raw); v != "" {
			clean = append(clean, v)
		}
	}
	if len(clean) == 0 {
		return lk.EgressInfo{}, fmt.Errorf("%w: minimal satu URL RTMP", domain.ErrValidation)
	}
	info, err := s.lkc.StartStream(ctx, lk.StreamOptions{Room: m.Room, Layout: layout, URLs: clean})
	if err != nil {
		return lk.EgressInfo{}, err
	}
	s.attachRecording(m.ID, domain.Recording{
		EgressID: info.EgressID, Kind: "stream", Status: info.Status, Layout: layout,
		URLs: clean, StartedAt: time.Now(), StartedBy: firstNonEmpty(u.Username, u.ID),
	})
	return info, nil
}

// StopEgress menghentikan satu egress; bila egressID kosong, semua egress aktif
// milik room dihentikan.
func (s *Service) StopEgress(ctx context.Context, u domain.User, id, egressID string) ([]lk.EgressInfo, error) {
	m, err := s.manageable(u, id)
	if err != nil {
		return nil, err
	}
	var targets []string
	if strings.TrimSpace(egressID) != "" {
		targets = []string{egressID}
	} else {
		list, lerr := s.lkc.ListEgress(ctx, m.Room, true)
		if lerr != nil {
			return nil, lerr
		}
		for _, e := range list {
			targets = append(targets, e.EgressID)
		}
	}
	out := make([]lk.EgressInfo, 0, len(targets))
	for _, t := range targets {
		info, serr := s.lkc.StopEgress(ctx, t)
		if serr != nil {
			return out, serr
		}
		s.updateRecording(m.ID, info)
		out = append(out, info)
	}
	return out, nil
}

// ListEgress mengembalikan egress room (aktif saja bila activeOnly).
func (s *Service) ListEgress(ctx context.Context, u domain.User, id string, activeOnly bool) ([]lk.EgressInfo, error) {
	m, err := s.GetMeeting(u, id)
	if err != nil {
		return nil, err
	}
	return s.lkc.ListEgress(ctx, m.Room, activeOnly)
}

// DispatchAgent menyuruh worker voice AI bergabung ke room meeting.
func (s *Service) DispatchAgent(ctx context.Context, u domain.User, id, agentName, metadata string) (string, error) {
	m, err := s.manageable(u, id)
	if err != nil {
		return "", err
	}
	name := firstNonEmpty(strings.TrimSpace(agentName), m.AgentName, s.opts.AgentName)
	if name == "" {
		return "", fmt.Errorf("%w: nama agent belum diatur (LIVEKIT_AGENT_NAME)", domain.ErrValidation)
	}
	dispatchID, err := s.lkc.DispatchAgent(ctx, lk.DispatchOptions{
		Room: m.Room, AgentName: name, Metadata: firstNonEmpty(metadata, m.AgentMetadata),
	})
	if err != nil {
		return "", err
	}
	_, _ = s.st.UpdateMeeting(m.ID, func(mm *domain.Meeting) error {
		mm.AgentEnabled = true
		mm.AgentName = name
		return nil
	})
	s.logEvent(m.Room, "agent.dispatched", name)
	return dispatchID, nil
}

// ---------- room mentah (direktur) ----------

// Rooms mengembalikan seluruh room aktif di server LiveKit.
func (s *Service) Rooms(ctx context.Context, u domain.User) ([]lk.RoomInfo, error) {
	if !u.IsDirector() {
		return nil, domain.ErrForbidden
	}
	return s.lkc.ListRooms(ctx)
}

// DeleteRoom menutup paksa sebuah room.
func (s *Service) DeleteRoom(ctx context.Context, u domain.User, room string) error {
	if !u.IsDirector() {
		return domain.ErrForbidden
	}
	if err := s.lkc.DeleteRoom(ctx, room); err != nil {
		return err
	}
	_, _ = s.st.UpdateMeetingByRoom(room, func(mm *domain.Meeting) error {
		mm.Status = domain.StatusEnded
		now := time.Now()
		mm.EndedAt = &now
		return nil
	})
	return nil
}

// ---------- webhook ----------

// ParseWebhook memverifikasi tanda tangan LiveKit dan mengurai event-nya.
func (s *Service) ParseWebhook(r *nethttp.Request) (lk.WebhookEvent, error) {
	return s.lkc.ReceiveWebhook(r)
}

// HandleWebhook memutakhirkan state dari event server LiveKit.
func (s *Service) HandleWebhook(ev lk.WebhookEvent) {
	detail := ev.Detail
	if ev.ParticipantIdentity != "" && detail == "" {
		detail = ev.ParticipantIdentity
	}
	s.logEvent(ev.Room, ev.Event, detail)

	switch ev.Event {
	case "room_started":
		_, _ = s.st.UpdateMeetingByRoom(ev.Room, func(m *domain.Meeting) error {
			m.Status = domain.StatusLive
			if m.StartedAt == nil {
				now := time.Now()
				m.StartedAt = &now
			}
			return nil
		})
	case "room_finished":
		_, _ = s.st.UpdateMeetingByRoom(ev.Room, func(m *domain.Meeting) error {
			m.Status = domain.StatusEnded
			now := time.Now()
			m.EndedAt = &now
			m.NumParticipants = 0
			return nil
		})
	case "participant_joined", "participant_left":
		_, _ = s.st.UpdateMeetingByRoom(ev.Room, func(m *domain.Meeting) error {
			m.NumParticipants = ev.NumParticipants
			return nil
		})
	case "egress_started", "egress_updated", "egress_ended":
		if m, err := s.st.GetMeetingByRoom(ev.Room); err == nil {
			s.updateRecording(m.ID, lk.EgressInfo{EgressID: ev.EgressID, Status: ev.EgressStatus, RoomName: ev.Room})
		}
	}

	if m, err := s.st.GetMeetingByRoom(ev.Room); err == nil {
		s.push("meeting.updated", m)
	}
	s.push("livekit.event", ev)
}

// Events mengembalikan jejak event terbaru (audit ringan).
func (s *Service) Events(u domain.User, room string, limit int) []domain.Event {
	if room != "" {
		if m, err := s.st.GetMeetingByRoom(room); err == nil && !m.Visible(u) {
			return nil
		}
	}
	return s.st.ListEvents(room, limit)
}

// Stats mengembalikan ringkasan jumlah meeting/event.
func (s *Service) Stats() map[string]int { return s.st.Stats() }

// ---------- util internal ----------

func (s *Service) manageable(u domain.User, id string) (domain.Meeting, error) {
	m, err := s.st.GetMeeting(id)
	if err != nil {
		return domain.Meeting{}, err
	}
	if !m.CanManage(u) {
		return domain.Meeting{}, domain.ErrForbidden
	}
	if !s.lkc.Enabled() {
		return m, domain.ErrDisabled
	}
	return m, nil
}

func (s *Service) attachRecording(meetingID string, rec domain.Recording) {
	saved, err := s.st.UpdateMeeting(meetingID, func(m *domain.Meeting) error {
		m.Recordings = append(m.Recordings, rec)
		return nil
	})
	if err == nil {
		s.push("meeting.updated", saved)
	}
}

func (s *Service) updateRecording(meetingID string, info lk.EgressInfo) {
	if info.EgressID == "" {
		return
	}
	saved, err := s.st.UpdateMeeting(meetingID, func(m *domain.Meeting) error {
		for i := range m.Recordings {
			if m.Recordings[i].EgressID != info.EgressID {
				continue
			}
			if info.Status != "" {
				m.Recordings[i].Status = info.Status
			}
			if info.Error != "" {
				m.Recordings[i].Error = info.Error
			}
			if info.Filename != "" {
				m.Recordings[i].Filepath = info.Filename
			}
			if strings.Contains(strings.ToUpper(info.Status), "COMPLETE") ||
				strings.Contains(strings.ToUpper(info.Status), "FAILED") ||
				strings.Contains(strings.ToUpper(info.Status), "ABORTED") {
				now := time.Now()
				m.Recordings[i].EndedAt = &now
			}
			return nil
		}
		return nil
	})
	if err == nil {
		s.push("meeting.updated", saved)
	}
}

func (s *Service) logEvent(room, kind, detail string) {
	_ = s.st.AppendEvent(domain.Event{Type: kind, Room: room, Detail: detail, At: time.Now()})
}

func identityOf(u domain.User) string {
	return firstNonEmpty(u.Username, u.ID, "anon")
}

func userMetadata(u domain.User, role domain.Role) string {
	b, err := json.Marshal(map[string]any{
		"userId":    u.ID,
		"username":  u.Username,
		"nama":      u.Name,
		"peran":     string(role),
		"divisi":    u.Divisions(),
		"direktur":  u.IsDirector(),
		"terbitJam": time.Now().Format(time.RFC3339),
	})
	if err != nil {
		return ""
	}
	return string(b)
}

func meetingMetadata(m domain.Meeting) string {
	b, err := json.Marshal(map[string]any{
		"meetingId": m.ID,
		"judul":     m.Title,
		"jenis":     string(m.Kind),
		"divisi":    m.Division,
	})
	if err != nil {
		return ""
	}
	return string(b)
}

func sourcesFor(role domain.Role) []string {
	if !role.CanPublish() {
		return nil
	}
	if role == domain.RoleAgent {
		return []string{"microphone"}
	}
	return []string{"camera", "microphone", "screen_share", "screen_share_audio"}
}

func normalizeInvitees(in []domain.Invitee) []domain.Invitee {
	out := make([]domain.Invitee, 0, len(in))
	for _, inv := range in {
		inv.Username = strings.TrimSpace(inv.Username)
		if inv.Username == "" && inv.UserID == "" {
			continue
		}
		if !inv.Role.Valid() {
			inv.Role = domain.RoleSpeaker
		}
		out = append(out, inv)
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
