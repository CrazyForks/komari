package cmd

import (
	"context"
	"errors"
	"io"
	"log"
	"math"
	"net"
	"strings"
	"time"

	apiClient "github.com/komari-monitor/komari/api/client"
	"github.com/komari-monitor/komari/common"
	"github.com/komari-monitor/komari/database/auditlog"
	"github.com/komari-monitor/komari/database/config"
	"github.com/komari-monitor/komari/database/dbcore"
	"github.com/komari-monitor/komari/database/models"
	"github.com/komari-monitor/komari/utils/geoip"
	"github.com/komari-monitor/komari/utils/notifier"
	"github.com/komari-monitor/komari/ws"

	"github.com/komari-monitor/komari/compat/nezha/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"gorm.io/gorm/clause"
)

// StartNezhaCompatServer starts a gRPC server compatible with Nezha Agent.
func StartNezhaCompatServer(addr string) error {
	boot := uint64(time.Now().Unix())
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &nezhaCompatServer{bootTime: boot}

	// Interceptors for metadata auth presence (lightweight; validation is inside handlers too)
	unary := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(ctx, req)
	}
	stream := func(srvIface interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srvIface, ss)
	}
	// Enable gRPC keepalive to tolerate long-idle streams
	gs := grpc.NewServer(
		grpc.UnaryInterceptor(unary),
		grpc.StreamInterceptor(stream),
		// Keepalive: allow client pings and keep connections stable without app traffic
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    2 * time.Minute,
			Timeout: 20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             20 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	proto.RegisterNezhaServiceServer(gs, srv)
	log.Printf("Nezha compat gRPC listening on %s", addr)
	return gs.Serve(lis)
}

type nezhaCompatServer struct {
	proto.UnimplementedNezhaServiceServer
	bootTime uint64
}

// authentication helpers
func getAuth(ctx context.Context) (uuid string, secret string, err error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", "", errors.New("missing metadata")
	}
	getFirst := func(key string) string {
		vals := md.Get(key)
		if len(vals) > 0 {
			return vals[0]
		}
		return ""
	}
	uuid = getFirst("client_uuid")
	secret = getFirst("client_secret")
	if uuid == "" || secret == "" {
		return "", "", errors.New("unauthorized: missing client_uuid/client_secret")
	}
	return uuid, secret, nil
}

// ReportSystemInfo: upsert static host info
func (s *nezhaCompatServer) ReportSystemInfo(ctx context.Context, host *proto.Host) (*proto.Receipt, error) {
	uuid, secret, err := getAuth(ctx)
	if err != nil {
		return nil, err
	}
	if err := upsertClientFromHost(uuid, secret, host); err != nil {
		return nil, err
	}
	return &proto.Receipt{Proced: true}, nil
}

// ReportSystemInfo2: same as above but returns dashboard boot time
func (s *nezhaCompatServer) ReportSystemInfo2(ctx context.Context, host *proto.Host) (*proto.Uint64Receipt, error) {
	uuid, secret, err := getAuth(ctx)
	if err != nil {
		return nil, err
	}
	if err := upsertClientFromHost(uuid, secret, host); err != nil {
		return nil, err
	}
	return &proto.Uint64Receipt{Data: s.bootTime}, nil
}

// ReportSystemState: ingest time-series records
func (s *nezhaCompatServer) ReportSystemState(stream proto.NezhaService_ReportSystemStateServer) error {
	ctx := stream.Context()
	uuid, _, err := getAuth(ctx)
	if err != nil {
		return err
	}
	// presence start
	connID := time.Now().UnixNano()
	ws.SetPresence(uuid, connID, true)
	go notifier.OnlineNotification(uuid, connID)
	defer func() {
		ws.SetPresence(uuid, connID, false)
		notifier.OfflineNotification(uuid, connID)
	}()
	for {
		st, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		// refresh presence TTL on every frame
		ws.KeepAlivePresence(uuid, connID, 30*time.Second)
		if err := ingestState(uuid, st); err != nil {
			// still ack to avoid client stuck; log error
			log.Printf("Nezha ingest state error: %v", err)
		}
		if err := stream.Send(&proto.Receipt{Proced: true}); err != nil {
			return err
		}
	}
}

// RequestTask: do not dispatch tasks, just drain results to avoid Unimplemented
func (s *nezhaCompatServer) RequestTask(stream proto.NezhaService_RequestTaskServer) error {
	ctx := stream.Context()
	uuid, _, err := getAuth(ctx)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	connID := time.Now().UnixNano()
	ws.SetPresence(uuid, connID, true)
	defer ws.SetPresence(uuid, connID, false)
	// receive results in background
	recvErr := make(chan error, 1)
	go func() {
		for {
			_, rerr := stream.Recv()
			if rerr == io.EOF {
				recvErr <- nil
				return
			}
			if rerr != nil {
				recvErr <- rerr
				return
			}
			// refresh presence TTL when result received
			ws.KeepAlivePresence(uuid, connID, 30*time.Second)
		}
	}()
	// send heartbeat tasks periodically
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-recvErr:
			return err
		case <-ticker.C:
			if err := stream.Send(&proto.Task{}); err != nil {
				return err
			}
		}
	}
}

// Unimplemented methods intentionally left as default (IOStream, ReportGeoIP)

// upsertClientFromHost maps Host into models.Client and upserts by UUID
func upsertClientFromHost(uuid, secret string, h *proto.Host) error {
	db := dbcore.GetDBInstance()
	now := models.FromTime(time.Now())
	// token guard: if existing record has different token, reject
	var exist models.Client
	if err := db.Where("uuid = ?", uuid).First(&exist).Error; err == nil {
		if exist.Token != "" && exist.Token != secret {
			return errors.New("unauthorized: token mismatch")
		}
	}
	cpuName := ""
	if len(h.Cpu) > 0 {
		cpuName = h.Cpu[0]
	}
	gpuName := strings.Join(h.Gpu, "; ")
	osName := h.Platform
	if h.PlatformVersion != "" {
		osName = h.Platform + " " + h.PlatformVersion
	}
	// clamp uint64 to int64 safely
	clamp := func(v uint64) int64 {
		if v > uint64(math.MaxInt64) {
			return math.MaxInt64
		}
		return int64(v)
	}
	c := models.Client{
		UUID:           uuid,
		Token:          secret,
		Name:           "nezha_" + uuid[0:8],
		CpuName:        cpuName,
		Arch:           h.Arch,
		CpuCores:       len(h.Cpu),
		OS:             osName,
		KernelVersion:  h.PlatformVersion,
		Virtualization: h.Virtualization,
		GpuName:        gpuName,
		MemTotal:       clamp(h.MemTotal),
		SwapTotal:      clamp(h.SwapTotal),
		DiskTotal:      clamp(h.DiskTotal),
		Version:        h.Version,
		UpdatedAt:      now,
	}
	// Upsert by UUID; don't override existing Token if already set
	updates := map[string]interface{}{
		"cpu_name":       c.CpuName,
		"arch":           c.Arch,
		"cpu_cores":      c.CpuCores,
		"os":             c.OS,
		"kernel_version": c.KernelVersion,
		"virtualization": c.Virtualization,
		"gpu_name":       c.GpuName,
		"mem_total":      c.MemTotal,
		"swap_total":     c.SwapTotal,
		"disk_total":     c.DiskTotal,
		"version":        c.Version,
		"updated_at":     time.Now(),
	}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "uuid"}},
		DoUpdates: clause.Assignments(updates),
	}).Create(&c).Error
}

// ingestState maps Nezha State into common.Report then saves a Record
func ingestState(uuid string, st *proto.State) error {
	// we may need totals from client
	db := dbcore.GetDBInstance()
	var client models.Client
	if err := db.Where("uuid = ?", uuid).First(&client).Error; err != nil {
		// If missing, create with minimal defaults to avoid failing ingestion
		client = models.Client{UUID: uuid, Token: "", Name: "nezha_" + uuid[0:8]}
		auditlog.EventLog("info", "auto created client "+client.Name)
		_ = db.Create(&client).Error
	}
	rep := common.Report{
		CPU:  common.CPUReport{Usage: st.Cpu},
		Ram:  common.RamReport{Total: client.MemTotal, Used: int64(st.MemUsed)},
		Swap: common.RamReport{Total: client.SwapTotal, Used: int64(st.SwapUsed)},
		Load: common.LoadReport{Load1: st.Load1, Load5: st.Load5, Load15: st.Load15},
		Disk: common.DiskReport{Total: client.DiskTotal, Used: int64(st.DiskUsed)},
		Network: common.NetworkReport{
			Up:        int64(st.NetOutSpeed),
			Down:      int64(st.NetInSpeed),
			TotalUp:   int64(st.NetOutTransfer),
			TotalDown: int64(st.NetInTransfer),
		},
		Uptime:    int64(st.Uptime),
		Process:   int(st.ProcessCount),
		UpdatedAt: time.Now(),
	}
	// 更新实时缓存供前端使用
	ws.SetLatestReport(uuid, &rep)
	// 写入内存缓存，入库交由定时聚合任务处理
	return apiClient.SaveClientReport(uuid, rep)
}

// ReportGeoIP: 保存 Agent 上报的 IP，并回填国家码/面板启动时间
func (s *nezhaCompatServer) ReportGeoIP(ctx context.Context, in *proto.GeoIP) (*proto.GeoIP, error) {
	uuid, _, err := getAuth(ctx)
	if err != nil {
		return nil, err
	}
	updates := map[string]interface{}{
		"updated_at": time.Now(),
	}
	var iso string
	if in != nil && in.Ip != nil {
		if v4 := strings.TrimSpace(in.Ip.Ipv4); v4 != "" {
			updates["ipv4"] = v4
			if cfg, err := config.Get(); err == nil && cfg.GeoIpEnabled {
				if ip := net.ParseIP(v4); ip != nil {
					if gi, _ := geoip.GetGeoInfo(ip); gi != nil {
						iso = gi.ISOCode
					}
				}
			}
		}
		if v6 := strings.TrimSpace(in.Ip.Ipv6); v6 != "" {
			updates["ipv6"] = v6
			if iso == "" { // 优先使用 v4 的国家码
				if cfg, err := config.Get(); err == nil && cfg.GeoIpEnabled {
					if ip := net.ParseIP(v6); ip != nil {
						if gi, _ := geoip.GetGeoInfo(ip); gi != nil {
							iso = gi.ISOCode
						}
					}
				}
			}
		}
	}
	if iso != "" {
		// UI 使用旗帜 emoji
		updates["region"] = geoip.GetRegionUnicodeEmoji(iso)
	}
	if len(updates) > 0 {
		_ = dbcore.GetDBInstance().Model(&models.Client{}).Where("uuid = ?", uuid).Updates(updates).Error
	}
	// 回写 GeoIP（包含国家码与面板启动时间）
	resp := &proto.GeoIP{Use6: in.GetUse6(), Ip: in.GetIp(), CountryCode: iso, DashboardBootTime: s.bootTime}
	return resp, nil
}
