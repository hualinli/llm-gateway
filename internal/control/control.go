package control

import (
	"context"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Server 控制面实现：内存热配置 + PG 持久化 + watch 广播。
// 写路径单一入口 ApplyConfig：先在 Store 落盘、成功后才推进内存版本，
// 保证“版本号单调”在控制面重启后依然成立（持久层是事实源，内存是快照）。
type Server struct {
	pb.UnimplementedConfigServiceServer
	cfg   atomic.Pointer[pb.Config]
	store Store

	mu       sync.Mutex
	watchers map[chan *pb.Config]struct{} // 每个 WatchConfig 流一个非阻塞投递口
}

// NewServer 创建并尝试从 Store 恢复最新配置；恢复失败视为启动期致命错误。
func NewServer(store Store) (*Server, error) {
	s := &Server{store: store, watchers: make(map[chan *pb.Config]struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	latest, err := store.LoadLatest(ctx)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		s.cfg.Store(latest)
		log.Printf("control: restored v%d from store", latest.Version)
	}
	return s, nil
}

// ApplyConfig 写入新配置：版本 = 当前 + 1，落盘成功后才对内生效并广播。
func (s *Server) ApplyConfig(keys map[string]string, routes map[string]*pb.ProviderList) (*pb.Config, error) {
	next := &pb.Config{Version: s.CurrentConfig().Version + 1, Keys: keys, Routes: routes}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.store.Save(ctx, next); err != nil {
		return nil, err // 持久化失败：版本不推进，不广播，调用方重试
	}
	s.cfg.Store(next)
	s.broadcast(next)
	log.Printf("control: apply version=%d keys=%d routes=%d", next.Version, len(keys), len(routes))
	return next, nil
}

func (s *Server) GetConfig(_ context.Context, _ *pb.GetRequest) (*pb.Config, error) {
	if c := s.cfg.Load(); c != nil {
		return c, nil
	}
	return &pb.Config{Version: 0, Keys: map[string]string{}, Routes: map[string]*pb.ProviderList{}}, nil
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
		case ch <- cfg:
		default: // 满：丢弃，重连补拉
		}
	}
}

func (s *Server) CurrentConfig() *pb.Config {
	if c := s.cfg.Load(); c != nil {
		return c
	}
	return &pb.Config{Version: 0}
}

// Run 启动 gRPC，addr 如 :50051
func Run(addr string, s *Server) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	gs := grpc.NewServer()
	pb.RegisterConfigServiceServer(gs, s)
	log.Printf("control listening on %s", addr)
	return gs.Serve(lis)
}

// Client 网关侧订阅端
type Client struct {
	conn   *grpc.ClientConn
	client pb.ConfigServiceClient
}

func Dial(addr string) (*Client, error) {
	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

func (c *Client) Close() { _ = c.conn.Close() }

// Helper：把 map[string][]string 转成 proto 的 map[string]*ProviderList
func RoutesToProto(m map[string][]string) map[string]*pb.ProviderList {
	out := make(map[string]*pb.ProviderList, len(m))
	for k, v := range m {
		out[k] = &pb.ProviderList{Providers: v}
	}
	return out
}

func RoutesFromProto(m map[string]*pb.ProviderList) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		if v != nil {
			out[k] = v.Providers
		}
	}
	return out
}
