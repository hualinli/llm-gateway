package control

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llm-gateway/internal/auth"
	pb "llm-gateway/proto/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

// Server 控制面实现：内存热配置 + PG 持久化 + watch 广播。
// 写路径单一入口 ApplyConfig：先在 Store 落盘、成功后才推进内存版本，
// 保证“版本号单调”在控制面重启后依然成立（持久层是事实源，内存是快照）。
type Server struct {
	pb.UnimplementedConfigServiceServer
	cfg   atomic.Pointer[pb.Config]
	store Store

	// applyMu serializes version allocation and persistence. Without it,
	// concurrent admin writes can compute the same next version.
	applyMu  sync.Mutex
	mu       sync.Mutex
	watchers map[chan *pb.Config]struct{} // 每个 WatchConfig 流一个 latest-snapshot 投递口

	// reports are observations from Gateway processes. They are not part of
	// the persisted desired snapshot and are lost when this process restarts.
	reportsMu sync.RWMutex
	reports   map[string]GatewayReport
}

// NewServer 创建并尝试从 Store 恢复最新配置；恢复失败视为启动期致命错误。
func NewServer(store Store) (*Server, error) {
	s := &Server{
		store:    store,
		watchers: make(map[chan *pb.Config]struct{}),
		reports:  make(map[string]GatewayReport),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	latest, err := store.LoadLatest(ctx)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		if err := validateKeyRecords(latest.KeyRecords); err != nil {
			return nil, fmt.Errorf("invalid restored credentials: %w", err)
		}
		if err := validateProviderConfigs(latest.Providers); err != nil {
			return nil, fmt.Errorf("invalid restored providers: %w", err)
		}
		latest = normalizeConfig(latest)
		s.cfg.Store(latest)
		log.Printf("control: restored v%d from store", latest.Version)
	}
	return s, nil
}

// ApplyConfig is the legacy raw-key compatibility entry point. It hashes the
// input immediately and persists only KeyRecords in the Config proto.
func (s *Server) ApplyConfig(keys map[string]string, routes map[string]*pb.ProviderList) (*pb.Config, error) {
	return s.ApplyKeyRecordsAndProviders(keyRecordsFromLegacy(keys), routes, nil)
}

// ApplyKeyRecords writes a safe credential snapshot: raw keys are never placed
// in the Config message, store payload, or Watch broadcast.
func (s *Server) ApplyKeyRecords(records []*pb.KeyRecord, routes map[string]*pb.ProviderList) (*pb.Config, error) {
	return s.ApplyKeyRecordsAndProviders(records, routes, nil)
}

// ApplyKeyRecordsAndProviders writes a complete versioned snapshot, including
// provider settings. Raw upstream credentials are never accepted in this
// message; ProviderConfig.CredentialRef is an opaque reference resolved by the
// Gateway locally.
func (s *Server) ApplyKeyRecordsAndProviders(records []*pb.KeyRecord, routes map[string]*pb.ProviderList, providers map[string]*pb.ProviderConfig) (*pb.Config, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	return s.applyKeyRecordsLocked(records, routes, providers)
}

var ErrConfigVersionConflict = errors.New("config version conflict")

// ErrInvalidConfig marks a caller-fixable snapshot (keys, routes, providers)
// so the HTTP admin API can return 400 instead of 500.
var ErrInvalidConfig = errors.New("invalid control config")

// ConfigPatch uses nil to preserve a field and an explicit empty collection
// to clear it. ExpectedVersion enables optimistic concurrency for admin edits.
type ConfigPatch struct {
	KeyRecords      []*pb.KeyRecord
	Routes          map[string]*pb.ProviderList
	Providers       map[string]*pb.ProviderConfig
	ExpectedVersion *int64
	LegacyKeys      map[string]string // local-demo compatibility; immediately hashed
}

// PatchConfig serializes the entire read/merge/persist operation with
// credential Create/Revoke. Reading defaults in the HTTP handler would let a
// route-only write accidentally restore an older credential snapshot.
func (s *Server) PatchConfig(patch ConfigPatch) (*pb.Config, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	current := s.CurrentConfig()
	if patch.ExpectedVersion != nil && *patch.ExpectedVersion != current.Version {
		return nil, fmt.Errorf("%w: expected %d, current %d", ErrConfigVersionConflict, *patch.ExpectedVersion, current.Version)
	}
	if patch.LegacyKeys != nil && patch.KeyRecords != nil {
		return nil, fmt.Errorf("%w: keys and key_records cannot be set together", ErrInvalidConfig)
	}
	records := patch.KeyRecords
	if patch.LegacyKeys != nil {
		records = keyRecordsFromLegacy(patch.LegacyKeys)
	} else if records == nil {
		records = current.KeyRecords
	}
	routes := patch.Routes
	if routes == nil {
		routes = current.Routes
	}
	providers := patch.Providers
	if providers == nil {
		providers = current.Providers
	}
	return s.applyKeyRecordsLocked(records, routes, providers)
}

func (s *Server) applyKeyRecordsLocked(records []*pb.KeyRecord, routes map[string]*pb.ProviderList, providers map[string]*pb.ProviderConfig) (*pb.Config, error) {
	if err := validateKeyRecords(records); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	if err := validateProviderConfigs(providers); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
	}
	next := cloneConfig(&pb.Config{Version: s.CurrentConfig().Version, KeyRecords: records, Routes: routes, Providers: normalizeProviderConfigSnapshot(providers)})
	next.Version++
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.store.Save(ctx, next); err != nil {
		return nil, err // 持久化失败：版本不推进，不广播，调用方重试
	}
	s.cfg.Store(next)
	s.broadcast(next)
	log.Printf("control: apply version=%d keys=%d routes=%d providers=%d", next.Version, len(records), len(routes), len(providers))
	return cloneConfig(next), nil
}

func (s *Server) GetConfig(_ context.Context, _ *pb.GetRequest) (*pb.Config, error) {
	if c := s.cfg.Load(); c != nil {
		return cloneConfig(c), nil
	}
	return &pb.Config{Version: 0, KeyRecords: []*pb.KeyRecord{}, Routes: map[string]*pb.ProviderList{}, Providers: map[string]*pb.ProviderConfig{}}, nil
}

// WatchConfig 真实推送：注册 watcher，先补发req之后已生效的版本，之后每次 ApplyConfig 即时送达。
// 慢消费者被跳过（非阻塞投递），重连时由 GetConfig 补拉，不会阻塞其他 watcher。
func (s *Server) WatchConfig(req *pb.WatchRequest, stream grpc.ServerStreamingServer[pb.Config]) error {
	ch := make(chan *pb.Config, 4)
	s.mu.Lock()
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.watchers, ch)
		s.mu.Unlock()
	}()

	last := req.GetVersion()
	if cur := s.cfg.Load(); cur != nil && cur.Version > last {
		if err := stream.Send(cur); err != nil {
			return err
		}
		last = cur.Version
	}
	for {
		select {
		case cfg := <-ch:
			if cfg.Version <= last { // 防御乱序/重复投递
				continue
			}
			if err := stream.Send(cfg); err != nil {
				return err
			}
			last = cfg.Version
		case <-stream.Context().Done():
			return nil
		}
	}
}

func (s *Server) broadcast(cfg *pb.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.watchers {
		select {
		case ch <- cloneConfig(cfg):
		default:
			// Keep the newest snapshot instead of dropping a version while
			// leaving the stream alive. The watcher will observe the latest
			// config and skip stale snapshots by version.
			for {
				select {
				case <-ch:
				default:
					break
				}
				if len(ch) == 0 {
					break
				}
			}
			select {
			case ch <- cloneConfig(cfg):
			default:
			}
		}
	}
}

func (s *Server) CurrentConfig() *pb.Config {
	if c := s.cfg.Load(); c != nil {
		return cloneConfig(c)
	}
	return &pb.Config{Version: 0, KeyRecords: []*pb.KeyRecord{}, Routes: map[string]*pb.ProviderList{}, Providers: map[string]*pb.ProviderConfig{}}
}

func cloneConfig(cfg *pb.Config) *pb.Config {
	if cfg == nil {
		return &pb.Config{Version: 0}
	}
	return proto.Clone(cfg).(*pb.Config)
}

func normalizeConfig(cfg *pb.Config) *pb.Config {
	out := cloneConfig(cfg)
	if len(out.KeyRecords) == 0 && len(out.Keys) > 0 {
		out.KeyRecords = keyRecordsFromLegacy(out.Keys)
	}
	out.Providers = normalizeProviderConfigSnapshot(out.Providers)
	// New snapshots never expose or persist the legacy raw-key field.
	out.Keys = nil
	return out
}

func normalizeProviderConfigSnapshot(configs map[string]*pb.ProviderConfig) map[string]*pb.ProviderConfig {
	out := make(map[string]*pb.ProviderConfig, len(configs))
	for id, cfg := range configs {
		if cfg == nil {
			out[id] = nil
			continue
		}
		copyCfg := proto.Clone(cfg).(*pb.ProviderConfig)
		if copyCfg.Id == "" {
			copyCfg.Id = id
		}
		out[id] = copyCfg
	}
	return out
}

func keyRecordsFromLegacy(keys map[string]string) []*pb.KeyRecord {
	legacy := auth.LegacyRecords(keys)
	out := make([]*pb.KeyRecord, 0, len(legacy))
	for _, record := range legacy {
		out = append(out, &pb.KeyRecord{
			KeyId:     record.ID,
			KeyHash:   record.Hash,
			KeyPrefix: record.Prefix,
			TenantId:  record.TenantID,
			Status:    record.Status,
		})
	}
	return out
}

func validateKeyRecords(records []*pb.KeyRecord) error {
	ids := make(map[string]struct{}, len(records))
	hashes := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record == nil {
			return fmt.Errorf("key record is nil")
		}
		if strings.TrimSpace(record.KeyId) == "" || strings.TrimSpace(record.TenantId) == "" {
			return fmt.Errorf("key record requires key_id and tenant_id")
		}
		if len(record.KeyHash) != 64 {
			return fmt.Errorf("key record %q has invalid hash length", record.KeyId)
		}
		if _, err := hex.DecodeString(record.KeyHash); err != nil {
			return fmt.Errorf("key record %q has invalid hash: %w", record.KeyId, err)
		}
		if record.Status != "" && record.Status != "active" && record.Status != "revoked" {
			return fmt.Errorf("key record %q has invalid status %q", record.KeyId, record.Status)
		}
		if _, ok := ids[record.KeyId]; ok {
			return fmt.Errorf("duplicate key_id %q", record.KeyId)
		}
		if _, ok := hashes[record.KeyHash]; ok {
			return fmt.Errorf("duplicate key_hash for key_id %q", record.KeyId)
		}
		ids[record.KeyId] = struct{}{}
		hashes[record.KeyHash] = struct{}{}
	}
	return nil
}

// ValidateProviderConfigs checks the control-plane portion of a provider
// snapshot without resolving local credentials. Secret material must remain
// behind CredentialRef and is intentionally not represented in this proto.
func ValidateProviderConfigs(configs map[string]*pb.ProviderConfig) error {
	return validateProviderConfigs(configs)
}

func validateProviderConfigs(configs map[string]*pb.ProviderConfig) error {
	seenIDs := make(map[string]struct{}, len(configs))
	for rawID, cfg := range configs {
		id := strings.TrimSpace(rawID)
		if id == "" {
			return fmt.Errorf("provider id is empty")
		}
		if id != rawID {
			return fmt.Errorf("provider id %q contains surrounding whitespace", rawID)
		}
		if _, exists := seenIDs[id]; exists {
			return fmt.Errorf("duplicate provider id %q", id)
		}
		seenIDs[id] = struct{}{}
		if cfg == nil {
			return fmt.Errorf("provider %q config is nil", id)
		}
		if cfg.Id != "" && strings.TrimSpace(cfg.Id) != id {
			return fmt.Errorf("provider %q has mismatched id %q", id, cfg.Id)
		}
		protocol := normalizeProviderProtocol(cfg.Protocol)
		switch protocol {
		case "mock":
			if cfg.CredentialRef != "" {
				return fmt.Errorf("provider %q mock cannot use credential_ref", id)
			}
		case "openai-compatible":
			providerURL := strings.TrimSpace(cfg.Url)
			if providerURL == "" {
				return fmt.Errorf("provider %q url is required", id)
			}
			parsed, err := url.Parse(providerURL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
				return fmt.Errorf("provider %q has invalid url", id)
			}
		default:
			return fmt.Errorf("provider %q has unsupported protocol %q", id, cfg.Protocol)
		}
		if strings.ContainsAny(cfg.CredentialRef, "\r\n") {
			return fmt.Errorf("provider %q credential_ref contains newline", id)
		}
		if strings.HasPrefix(strings.TrimSpace(cfg.CredentialRef), "sk-") {
			return fmt.Errorf("provider %q credential_ref looks like a raw secret", id)
		}
		if strings.ContainsAny(cfg.Model, "\r\n") {
			return fmt.Errorf("provider %q model contains newline", id)
		}
		for source, target := range cfg.ModelMapping {
			if strings.TrimSpace(source) == "" || strings.TrimSpace(target) == "" {
				return fmt.Errorf("provider %q model_mapping contains empty key or value", id)
			}
		}
		if cfg.MockDelayMs < 0 {
			return fmt.Errorf("provider %q mock_delay_ms must be non-negative", id)
		}
		timeouts := cfg.Timeouts
		if timeouts == nil {
			continue
		}
		if timeouts.ConnectTimeoutMs < 0 || timeouts.TlsHandshakeTimeoutMs < 0 || timeouts.ResponseHeaderTimeoutMs < 0 || timeouts.FirstByteTimeoutMs < 0 || timeouts.IdleTimeoutMs < 0 || timeouts.AttemptTimeoutMs < 0 {
			return fmt.Errorf("provider %q timeout must be non-negative", id)
		}
		if timeouts.MaxEventBytes < 0 {
			return fmt.Errorf("provider %q max_event_bytes must be non-negative", id)
		}
	}
	return nil
}

func normalizeProviderProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "openai", "openai_compatible", "openai-compatible":
		return "openai-compatible"
	case "mock":
		return "mock"
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

func KeyRecordsToAuth(records []*pb.KeyRecord) []auth.KeyRecord {
	out := make([]auth.KeyRecord, 0, len(records))
	for _, record := range records {
		if record == nil {
			continue
		}
		out = append(out, auth.KeyRecord{
			ID:       record.KeyId,
			Hash:     record.KeyHash,
			Prefix:   record.KeyPrefix,
			TenantID: record.TenantId,
			Status:   record.Status,
		})
	}
	return out
}

// NewGRPCServer 构造控制面 gRPC server，便于进程入口和网络集成测试复用。
func NewGRPCServer(s *Server) *grpc.Server {
	gs, err := NewGRPCServerWithTLS(s, nil)
	if err != nil {
		panic(err)
	}
	return gs
}

// NewGRPCServerWithTLS constructs an insecure or TLS/mTLS gRPC server.
func NewGRPCServerWithTLS(s *Server, cfg *TLSConfig) (*grpc.Server, error) {
	serverCreds, err := serverCredentials(cfg)
	if err != nil {
		return nil, err
	}
	options := make([]grpc.ServerOption, 0, 1)
	if serverCreds != nil {
		options = append(options, grpc.Creds(serverCreds))
	}
	gs := grpc.NewServer(options...)
	pb.RegisterConfigServiceServer(gs, s)
	return gs, nil
}

// Serve 在已有 listener 上启动 insecure gRPC。
func Serve(lis net.Listener, s *Server) error {
	return ServeWithTLS(lis, s, nil)
}

// ServeWithTLS 在已有 listener 上启动 TLS/mTLS gRPC。
func ServeWithTLS(lis net.Listener, s *Server, cfg *TLSConfig) error {
	gs, err := NewGRPCServerWithTLS(s, cfg)
	if err != nil {
		return err
	}
	log.Printf("control listening on %s", lis.Addr())
	return gs.Serve(lis)
}

// Run 启动 gRPC，addr 如 :50051。
func Run(addr string, s *Server) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return Serve(lis, s)
}

// RunWithTLS 启动 TLS/mTLS gRPC，addr 如 :50051。
func RunWithTLS(addr string, s *Server, cfg *TLSConfig) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return ServeWithTLS(lis, s, cfg)
}

// Client 网关侧订阅端
type Client struct {
	conn   *grpc.ClientConn
	client pb.ConfigServiceClient
}

func Dial(addr string) (*Client, error) {
	conn, err := grpc.Dial(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                15 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, client: pb.NewConfigServiceClient(conn)}, nil
}

// DialContext is the bounded, blocking insecure variant used by local demos.
func DialContext(ctx context.Context, addr string) (*Client, error) {
	return DialContextWithTLS(ctx, addr, nil)
}

// DialContextWithTLS dials the control plane with TLS/mTLS when configured.
func DialContextWithTLS(ctx context.Context, addr string, cfg *TLSConfig) (*Client, error) {
	transportCreds, err := clientCredentials(cfg)
	if err != nil {
		return nil, err
	}
	if transportCreds == nil {
		transportCreds = insecure.NewCredentials()
	}
	conn, err := grpc.DialContext(ctx, addr,
		grpc.WithTransportCredentials(transportCreds),
		grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                15 * time.Second,
			Timeout:             5 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn, client: pb.NewConfigServiceClient(conn)}, nil
}

func (c *Client) GetConfig(ctx context.Context) (*pb.Config, error) {
	return c.client.GetConfig(ctx, &pb.GetRequest{})
}

func (c *Client) WatchConfig(ctx context.Context, version int64) (pb.ConfigService_WatchConfigClient, error) {
	return c.client.WatchConfig(ctx, &pb.WatchRequest{Version: version})
}

func (c *Client) ReportGatewayStatus(ctx context.Context, req *pb.ReportGatewayStatusRequest) (*pb.ReportGatewayStatusResponse, error) {
	return c.client.ReportGatewayStatus(ctx, req)
}

func (c *Client) Close() { _ = c.conn.Close() }

// Helper：把 map[string][]string 转成 proto 的 map[string]*ProviderList
func RoutesToProto(m map[string][]string) map[string]*pb.ProviderList {
	out := make(map[string]*pb.ProviderList, len(m))
	for k, v := range m {
		out[k] = &pb.ProviderList{Providers: append([]string(nil), v...)}
	}
	return out
}

func RoutesFromProto(m map[string]*pb.ProviderList) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = append([]string(nil), v.Providers...)
		}
	}
	return out
}
