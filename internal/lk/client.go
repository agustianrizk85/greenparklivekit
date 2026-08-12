// Package lk membungkus LiveKit server SDK menjadi API internal yang ringkas.
//
// Semua tipe yang dikembalikan ke pemanggil adalah DTO polos (bukan protobuf)
// supaya hasil JSON-nya rapi dan tidak terikat versi protokol LiveKit.
package lk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
	lksdk "github.com/livekit/server-sdk-go/v2"
)

// ErrDisabled dikembalikan semua method saat kredensial LiveKit belum diisi.
var ErrDisabled = errors.New("LiveKit belum dikonfigurasi: isi LIVEKIT_API_KEY dan LIVEKIT_API_SECRET")

// defaultTokenTTL adalah masa berlaku token bawaan jika Config.TokenTTL kosong.
const defaultTokenTTL = 4 * time.Hour

// Config memuat kredensial dan alamat server LiveKit.
type Config struct {
	URL       string
	APIKey    string
	APISecret string
	TokenTTL  time.Duration
}

// Client adalah fasad tipis di atas LiveKit server SDK.
type Client struct {
	cfg      Config
	ttl      time.Duration
	enabled  bool
	rooms    *lksdk.RoomServiceClient
	egress   *lksdk.EgressClient
	dispatch *lksdk.AgentDispatchClient
	keys     auth.KeyProvider
}

// New membuat Client dan tidak pernah mengembalikan error. Jika APIKey atau
// APISecret kosong, Client tetap dapat dipakai tetapi Enabled() bernilai false
// dan semua method mengembalikan ErrDisabled.
func New(cfg Config) *Client {
	c := &Client{cfg: cfg, ttl: cfg.TokenTTL}
	if c.ttl <= 0 {
		c.ttl = defaultTokenTTL
	}
	if cfg.APIKey == "" || cfg.APISecret == "" {
		return c
	}
	c.enabled = true
	// SDK otomatis mengubah ws:// dan wss:// menjadi http(s):// untuk API twirp.
	c.rooms = lksdk.NewRoomServiceClient(cfg.URL, cfg.APIKey, cfg.APISecret)
	c.egress = lksdk.NewEgressClient(cfg.URL, cfg.APIKey, cfg.APISecret)
	c.dispatch = lksdk.NewAgentDispatchServiceClient(cfg.URL, cfg.APIKey, cfg.APISecret)
	c.keys = auth.NewSimpleKeyProvider(cfg.APIKey, cfg.APISecret)
	return c
}

// Enabled melaporkan apakah kredensial LiveKit tersedia.
func (c *Client) Enabled() bool { return c != nil && c.enabled }

// URL mengembalikan URL LiveKit apa adanya, siap dikirim ke frontend.
func (c *Client) URL() string { return c.cfg.URL }

// Grant mendeskripsikan izin yang dituangkan ke dalam access token.
type Grant struct {
	Room, Identity, Name, Metadata                                 string
	TTL                                                            time.Duration // 0 = pakai Config.TokenTTL (default 4 jam)
	CanPublish, CanSubscribe, CanPublishData, CanUpdateOwnMetadata bool
	RoomAdmin, RoomCreate, RoomList, Hidden, Recorder, Agent       bool
	Sources                                                        []string // "camera","microphone","screen_share","screen_share_audio"
	Attributes                                                     map[string]string
}

// RoomInfo adalah ringkasan sebuah room.
// Tag JSON ditulis eksplisit (camelCase) karena DTO ini langsung menjadi badan
// respons API — tanpa tag, Go mengekspornya sebagai PascalCase dan frontend
// harus menulis `room.Name` di tengah kode yang seluruhnya camelCase.
type RoomInfo struct {
	Name            string `json:"name"`
	SID             string `json:"sid"`
	Metadata        string `json:"metadata,omitempty"`
	NumParticipants uint32 `json:"numParticipants"`
	NumPublishers   uint32 `json:"numPublishers"`
	MaxParticipants uint32 `json:"maxParticipants,omitempty"`
	EmptyTimeout    uint32 `json:"emptyTimeout,omitempty"`
	CreatedAt       int64  `json:"createdAt"` // unix detik
	ActiveRecording bool   `json:"activeRecording"`
}

// TrackInfo adalah ringkasan sebuah track yang dipublikasikan.
type TrackInfo struct {
	SID    string `json:"sid"`
	Type   string `json:"type"`
	Source string `json:"source"`
	Name   string `json:"name,omitempty"`
	Muted  bool   `json:"muted"`
}

// ParticipantInfo adalah ringkasan seorang peserta di dalam room.
type ParticipantInfo struct {
	SID         string            `json:"sid"`
	Identity    string            `json:"identity"`
	Name        string            `json:"name,omitempty"`
	State       string            `json:"state"`
	Metadata    string            `json:"metadata,omitempty"`
	JoinedAt    int64             `json:"joinedAt"`
	IsPublisher bool              `json:"isPublisher"`
	Attributes  map[string]string `json:"attributes,omitempty"`
	Tracks      []TrackInfo       `json:"tracks,omitempty"`
}

// EgressInfo adalah ringkasan proses egress (streaming atau perekaman).
type EgressInfo struct {
	EgressID  string   `json:"egressId"`
	RoomName  string   `json:"roomName"`
	Status    string   `json:"status"`
	Error     string   `json:"error,omitempty"`
	StartedAt int64    `json:"startedAt"`
	EndedAt   int64    `json:"endedAt,omitempty"`
	Filename  string   `json:"filename,omitempty"`
	URLs      []string `json:"urls,omitempty"`
}

// WebhookEvent adalah bentuk datar dari webhook LiveKit.
type WebhookEvent struct {
	ID                  string `json:"id"`
	Event               string `json:"event"`
	Room                string `json:"room,omitempty"`
	ParticipantIdentity string `json:"participantIdentity,omitempty"`
	ParticipantName     string `json:"participantName,omitempty"`
	EgressID            string `json:"egressId,omitempty"`
	EgressStatus        string `json:"egressStatus,omitempty"`
	TrackSID            string `json:"trackSid,omitempty"`
	Detail              string `json:"detail,omitempty"`
	NumParticipants     int    `json:"numParticipants"`
	CreatedAt           int64  `json:"createdAt"`
}

// Token menerbitkan access token JWT untuk peserta beserta waktu kedaluwarsanya.
func (c *Client) Token(g Grant) (string, time.Time, error) {
	if !c.Enabled() {
		return "", time.Time{}, ErrDisabled
	}
	if strings.TrimSpace(g.Identity) == "" {
		return "", time.Time{}, errors.New("identity wajib diisi")
	}

	grant := &auth.VideoGrant{
		Room:       g.Room,
		RoomAdmin:  g.RoomAdmin,
		RoomCreate: g.RoomCreate,
		RoomList:   g.RoomList,
		Hidden:     g.Hidden,
		Recorder:   g.Recorder,
		Agent:      g.Agent,
	}
	if g.Room != "" {
		grant.RoomJoin = true
	}
	grant.SetCanPublish(g.CanPublish)
	grant.SetCanSubscribe(g.CanSubscribe)
	grant.SetCanPublishData(g.CanPublishData)
	grant.SetCanUpdateOwnMetadata(g.CanUpdateOwnMetadata)
	if srcs := parseSources(g.Sources); len(srcs) > 0 {
		grant.SetCanPublishSources(srcs)
	}

	ttl := g.TTL
	if ttl <= 0 {
		ttl = c.ttl
	}

	at := auth.NewAccessToken(c.cfg.APIKey, c.cfg.APISecret).
		SetVideoGrant(grant).
		SetIdentity(g.Identity).
		SetValidFor(ttl)
	if g.Name != "" {
		at = at.SetName(g.Name)
	}
	if g.Metadata != "" {
		at = at.SetMetadata(g.Metadata)
	}
	if len(g.Attributes) > 0 {
		at = at.SetAttributes(g.Attributes)
	}

	tok, err := at.ToJWT()
	if err != nil {
		return "", time.Time{}, fmt.Errorf("gagal membuat token: %w", err)
	}
	return tok, time.Now().Add(ttl), nil
}

// parseSources menerjemahkan nama sumber track; nilai tak dikenal diabaikan.
func parseSources(names []string) []livekit.TrackSource {
	out := make([]livekit.TrackSource, 0, len(names))
	for _, n := range names {
		switch strings.ToLower(strings.TrimSpace(n)) {
		case "camera":
			out = append(out, livekit.TrackSource_CAMERA)
		case "microphone":
			out = append(out, livekit.TrackSource_MICROPHONE)
		case "screen_share":
			out = append(out, livekit.TrackSource_SCREEN_SHARE)
		case "screen_share_audio":
			out = append(out, livekit.TrackSource_SCREEN_SHARE_AUDIO)
		}
	}
	return out
}

// RoomOptions adalah parameter pembuatan room.
type RoomOptions struct {
	Name, Metadata                                  string
	EmptyTimeout, DepartureTimeout, MaxParticipants uint32
}

// CreateRoom membuat room baru (idempoten di sisi server LiveKit).
func (c *Client) CreateRoom(ctx context.Context, o RoomOptions) (RoomInfo, error) {
	if !c.Enabled() {
		return RoomInfo{}, ErrDisabled
	}
	if strings.TrimSpace(o.Name) == "" {
		return RoomInfo{}, errors.New("nama room wajib diisi")
	}
	room, err := c.rooms.CreateRoom(ctx, &livekit.CreateRoomRequest{
		Name:             o.Name,
		Metadata:         o.Metadata,
		EmptyTimeout:     o.EmptyTimeout,
		DepartureTimeout: o.DepartureTimeout,
		MaxParticipants:  o.MaxParticipants,
	})
	if err != nil {
		return RoomInfo{}, fmt.Errorf("gagal membuat room: %w", err)
	}
	return toRoomInfo(room), nil
}

// ListRooms mengambil daftar room aktif; names opsional sebagai filter.
func (c *Client) ListRooms(ctx context.Context, names ...string) ([]RoomInfo, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	res, err := c.rooms.ListRooms(ctx, &livekit.ListRoomsRequest{Names: names})
	if err != nil {
		return nil, fmt.Errorf("gagal mengambil daftar room: %w", err)
	}
	out := make([]RoomInfo, 0, len(res.GetRooms()))
	for _, r := range res.GetRooms() {
		out = append(out, toRoomInfo(r))
	}
	return out, nil
}

// DeleteRoom menutup room dan memutus semua peserta di dalamnya.
func (c *Client) DeleteRoom(ctx context.Context, room string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	if _, err := c.rooms.DeleteRoom(ctx, &livekit.DeleteRoomRequest{Room: room}); err != nil {
		return fmt.Errorf("gagal menghapus room: %w", err)
	}
	return nil
}

// ListParticipants mengambil peserta yang sedang berada di dalam room.
func (c *Client) ListParticipants(ctx context.Context, room string) ([]ParticipantInfo, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	res, err := c.rooms.ListParticipants(ctx, &livekit.ListParticipantsRequest{Room: room})
	if err != nil {
		return nil, fmt.Errorf("gagal mengambil daftar peserta: %w", err)
	}
	out := make([]ParticipantInfo, 0, len(res.GetParticipants()))
	for _, p := range res.GetParticipants() {
		out = append(out, toParticipantInfo(p))
	}
	return out, nil
}

// RemoveParticipant mengeluarkan peserta dari room.
func (c *Client) RemoveParticipant(ctx context.Context, room, identity string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	_, err := c.rooms.RemoveParticipant(ctx, &livekit.RoomParticipantIdentity{Room: room, Identity: identity})
	if err != nil {
		return fmt.Errorf("gagal mengeluarkan peserta: %w", err)
	}
	return nil
}

// MuteTrack membisukan atau membunyikan kembali track milik peserta.
func (c *Client) MuteTrack(ctx context.Context, room, identity, trackSID string, muted bool) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	_, err := c.rooms.MutePublishedTrack(ctx, &livekit.MuteRoomTrackRequest{
		Room:     room,
		Identity: identity,
		TrackSid: trackSID,
		Muted:    muted,
	})
	if err != nil {
		return fmt.Errorf("gagal mengubah status mute track: %w", err)
	}
	return nil
}

// SendData mengirim pesan data ke room. Jika identities kosong, pesan
// disiarkan ke seluruh peserta.
func (c *Client) SendData(ctx context.Context, room string, payload []byte, identities []string) error {
	if !c.Enabled() {
		return ErrDisabled
	}
	_, err := c.rooms.SendData(ctx, &livekit.SendDataRequest{
		Room:                  room,
		Data:                  payload,
		Kind:                  livekit.DataPacket_RELIABLE,
		DestinationIdentities: identities,
	})
	if err != nil {
		return fmt.Errorf("gagal mengirim data: %w", err)
	}
	return nil
}

// StreamOptions adalah parameter siaran room ke server RTMP/SRT/WebSocket.
type StreamOptions struct {
	Room, Layout string
	URLs         []string
	AudioOnly    bool
}

// StartStream memulai room composite egress dengan keluaran stream.
func (c *Client) StartStream(ctx context.Context, o StreamOptions) (EgressInfo, error) {
	if !c.Enabled() {
		return EgressInfo{}, ErrDisabled
	}
	if strings.TrimSpace(o.Room) == "" {
		return EgressInfo{}, errors.New("nama room wajib diisi")
	}
	if len(o.URLs) == 0 {
		return EgressInfo{}, errors.New("minimal satu URL tujuan stream wajib diisi")
	}
	req := &livekit.RoomCompositeEgressRequest{
		RoomName:  o.Room,
		Layout:    layoutOrDefault(o.Layout),
		AudioOnly: o.AudioOnly,
		StreamOutputs: []*livekit.StreamOutput{{
			Protocol: streamProtocol(o.URLs),
			Urls:     o.URLs,
		}},
		Options: &livekit.RoomCompositeEgressRequest_Preset{
			Preset: livekit.EncodingOptionsPreset_H264_1080P_30,
		},
	}
	info, err := c.egress.StartRoomCompositeEgress(ctx, req)
	if err != nil {
		return EgressInfo{}, fmt.Errorf("gagal memulai stream: %w", err)
	}
	return toEgressInfo(info), nil
}

// RecordOptions adalah parameter perekaman room ke berkas MP4.
type RecordOptions struct {
	Room, Layout, Filepath string
	AudioOnly              bool
}

// StartRecord memulai room composite egress dengan keluaran berkas MP4.
func (c *Client) StartRecord(ctx context.Context, o RecordOptions) (EgressInfo, error) {
	if !c.Enabled() {
		return EgressInfo{}, ErrDisabled
	}
	if strings.TrimSpace(o.Room) == "" {
		return EgressInfo{}, errors.New("nama room wajib diisi")
	}
	req := &livekit.RoomCompositeEgressRequest{
		RoomName:  o.Room,
		Layout:    layoutOrDefault(o.Layout),
		AudioOnly: o.AudioOnly,
		FileOutputs: []*livekit.EncodedFileOutput{{
			FileType: livekit.EncodedFileType_MP4,
			Filepath: o.Filepath,
		}},
		Options: &livekit.RoomCompositeEgressRequest_Preset{
			Preset: livekit.EncodingOptionsPreset_H264_1080P_30,
		},
	}
	info, err := c.egress.StartRoomCompositeEgress(ctx, req)
	if err != nil {
		return EgressInfo{}, fmt.Errorf("gagal memulai perekaman: %w", err)
	}
	return toEgressInfo(info), nil
}

// ListEgress mengambil daftar egress; room opsional sebagai filter dan
// activeOnly membatasi hasil ke egress yang masih berjalan.
func (c *Client) ListEgress(ctx context.Context, room string, activeOnly bool) ([]EgressInfo, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	res, err := c.egress.ListEgress(ctx, &livekit.ListEgressRequest{RoomName: room, Active: activeOnly})
	if err != nil {
		return nil, fmt.Errorf("gagal mengambil daftar egress: %w", err)
	}
	out := make([]EgressInfo, 0, len(res.GetItems()))
	for _, it := range res.GetItems() {
		out = append(out, toEgressInfo(it))
	}
	return out, nil
}

// StopEgress menghentikan egress yang sedang berjalan.
func (c *Client) StopEgress(ctx context.Context, egressID string) (EgressInfo, error) {
	if !c.Enabled() {
		return EgressInfo{}, ErrDisabled
	}
	info, err := c.egress.StopEgress(ctx, &livekit.StopEgressRequest{EgressId: egressID})
	if err != nil {
		return EgressInfo{}, fmt.Errorf("gagal menghentikan egress: %w", err)
	}
	return toEgressInfo(info), nil
}

// DispatchOptions adalah parameter pemanggilan agent (voice AI) ke sebuah room.
type DispatchOptions struct {
	Room, AgentName, Metadata string
}

// DispatchAgent menugaskan agent ke room dan mengembalikan ID dispatch-nya.
func (c *Client) DispatchAgent(ctx context.Context, o DispatchOptions) (string, error) {
	if !c.Enabled() {
		return "", ErrDisabled
	}
	if strings.TrimSpace(o.Room) == "" {
		return "", errors.New("nama room wajib diisi")
	}
	if strings.TrimSpace(o.AgentName) == "" {
		return "", errors.New("agent name wajib diisi")
	}
	d, err := c.dispatch.CreateDispatch(ctx, &livekit.CreateAgentDispatchRequest{
		Room:      o.Room,
		AgentName: o.AgentName,
		Metadata:  o.Metadata,
	})
	if err != nil {
		return "", fmt.Errorf("gagal menugaskan agent: %w", err)
	}
	return d.GetId(), nil
}

// ReceiveWebhook memverifikasi tanda tangan lalu mendatarkan webhook LiveKit.
func (c *Client) ReceiveWebhook(r *http.Request) (WebhookEvent, error) {
	if !c.Enabled() {
		return WebhookEvent{}, ErrDisabled
	}
	ev, err := webhook.ReceiveWebhookEvent(r, c.keys)
	if err != nil {
		return WebhookEvent{}, fmt.Errorf("webhook tidak valid: %w", err)
	}
	out := WebhookEvent{
		ID:        ev.GetId(),
		Event:     ev.GetEvent(),
		CreatedAt: ev.GetCreatedAt(),
	}
	if room := ev.GetRoom(); room != nil {
		out.Room = room.GetName()
		out.NumParticipants = int(room.GetNumParticipants())
	}
	if p := ev.GetParticipant(); p != nil {
		out.ParticipantIdentity = p.GetIdentity()
		out.ParticipantName = p.GetName()
	}
	if t := ev.GetTrack(); t != nil {
		out.TrackSID = t.GetSid()
	}
	if eg := ev.GetEgressInfo(); eg != nil {
		out.EgressID = eg.GetEgressId()
		out.EgressStatus = eg.GetStatus().String()
		out.Detail = firstNonEmpty(eg.GetDetails(), eg.GetError())
	}
	return out, nil
}

// toRoomInfo mengubah protobuf Room menjadi DTO.
func toRoomInfo(r *livekit.Room) RoomInfo {
	if r == nil {
		return RoomInfo{}
	}
	return RoomInfo{
		Name:            r.GetName(),
		SID:             r.GetSid(),
		Metadata:        r.GetMetadata(),
		NumParticipants: r.GetNumParticipants(),
		NumPublishers:   r.GetNumPublishers(),
		MaxParticipants: r.GetMaxParticipants(),
		EmptyTimeout:    r.GetEmptyTimeout(),
		CreatedAt:       r.GetCreationTime(),
		ActiveRecording: r.GetActiveRecording(),
	}
}

// toParticipantInfo mengubah protobuf ParticipantInfo menjadi DTO.
func toParticipantInfo(p *livekit.ParticipantInfo) ParticipantInfo {
	if p == nil {
		return ParticipantInfo{}
	}
	out := ParticipantInfo{
		SID:         p.GetSid(),
		Identity:    p.GetIdentity(),
		Name:        p.GetName(),
		State:       p.GetState().String(),
		Metadata:    p.GetMetadata(),
		JoinedAt:    p.GetJoinedAt(),
		IsPublisher: p.GetIsPublisher(),
		Attributes:  p.GetAttributes(),
		Tracks:      make([]TrackInfo, 0, len(p.GetTracks())),
	}
	for _, t := range p.GetTracks() {
		out.Tracks = append(out.Tracks, TrackInfo{
			SID:    t.GetSid(),
			Type:   t.GetType().String(),
			Source: t.GetSource().String(),
			Name:   t.GetName(),
			Muted:  t.GetMuted(),
		})
	}
	return out
}

// toEgressInfo mengubah protobuf EgressInfo menjadi DTO, mengambil nama berkas
// dan URL stream dari hasil egress bila sudah tersedia.
func toEgressInfo(e *livekit.EgressInfo) EgressInfo {
	if e == nil {
		return EgressInfo{}
	}
	out := EgressInfo{
		EgressID:  e.GetEgressId(),
		RoomName:  e.GetRoomName(),
		Status:    e.GetStatus().String(),
		Error:     e.GetError(),
		StartedAt: e.GetStartedAt(),
		EndedAt:   e.GetEndedAt(),
	}
	for _, f := range e.GetFileResults() {
		if out.Filename = firstNonEmpty(f.GetFilename(), f.GetLocation()); out.Filename != "" {
			break
		}
	}
	for _, s := range e.GetStreamResults() {
		if s.GetUrl() != "" {
			out.URLs = append(out.URLs, s.GetUrl())
		}
	}
	return out
}

// layoutOrDefault memakai layout "grid" bila tidak ditentukan.
func layoutOrDefault(layout string) string {
	if strings.TrimSpace(layout) == "" {
		return "grid"
	}
	return layout
}

// streamProtocol menebak protokol stream dari skema URL pertama.
func streamProtocol(urls []string) livekit.StreamProtocol {
	for _, u := range urls {
		switch {
		case strings.HasPrefix(u, "rtmp://"), strings.HasPrefix(u, "rtmps://"):
			return livekit.StreamProtocol_RTMP
		case strings.HasPrefix(u, "srt://"):
			return livekit.StreamProtocol_SRT
		case strings.HasPrefix(u, "ws://"), strings.HasPrefix(u, "wss://"):
			return livekit.StreamProtocol_WEBSOCKET
		}
	}
	return livekit.StreamProtocol_DEFAULT_PROTOCOL
}

// firstNonEmpty mengembalikan string pertama yang tidak kosong.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
