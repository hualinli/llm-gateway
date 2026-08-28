package control

import (
	"context"
	"log"
	"net"
	"sync/atomic"

	pb "llm-gateway/proto/control"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Server 控制面实现，基于 protoc 生成的 ConfigService
type Server struct {
	pb.UnimplementedConfigServiceServer
	cfg atomic.Pointer[pb.Config]
}

func (s *Server) GetConfig(_ context.Context, _ *pb.GetRequest) (*pb.Config, error) {
	if c := s.cfg.Load(); c != nil {
		return c, nil
	}
	return &pb.Config{Version: 1, Keys: map[string]string{}, Routes: map[string]*pb.ProviderList{}}, nil
}

func (s *Server) WatchConfig(req *pb.WatchRequest, stream grpc.ServerStreamingServer[pb.Config]) error {
	cfg, _ := s.GetConfig(stream.Context(), &pb.GetRequest{})
	if cfg.Version > req.Version {
		if err := stream.Send(cfg); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return nil
}

func (s *Server) UpdateConfig(c *pb.Config) {
	s.cfg.Store(c)
	log.Printf("control: update version=%d keys=%d routes=%d", c.Version, len(c.Keys), len(c.Routes))
}

func (s *Server) CurrentConfig() *pb.Config {
	if c := s.cfg.Load(); c != nil {
		return c
	}
	return &pb.Config{Version: 1}
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
