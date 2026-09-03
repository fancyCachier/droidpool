// droidpoold 是 droidpool 的控制面：管理节点上的 redroid 容器、发放租约、
// 提供 HTTP API 与设备墙。跑在 devopt，通过 docker over SSH 控制节点。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fancyCachier/droidpool/internal/adb"
	"github.com/fancyCachier/droidpool/internal/api"
	"github.com/fancyCachier/droidpool/internal/config"
	"github.com/fancyCachier/droidpool/internal/node"
	"github.com/fancyCachier/droidpool/internal/pool"
	"github.com/fancyCachier/droidpool/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("c", "config.toml", "配置文件路径")
	ensure := flag.Bool("ensure", true, "启动时把池补齐到 max_devices")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("打开数据库: %w", err)
	}
	defer st.Close()

	// 目前是单节点。多节点时这里改成遍历，Manager 与 Health 各一份。
	nc := cfg.Nodes[0]
	nd := &node.Node{
		Name: nc.Name, DockerHost: nc.DockerHost, ADBHost: nc.ADBHost,
		Image: nc.Image, DataRoot: nc.DataRoot, BootArgs: nc.BootArgs,
	}
	mgr := &pool.Manager{
		NodeName: nc.Name, ADBHost: nc.ADBHost, Driver: nd, Store: st,
		MaxDevices: nc.MaxDevices, PortBase: nc.PortRange[0] - 1,
		OverlayBase: nc.DataRoot + "/base", Log: log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 设备墙的截图与输入都走本机 adb
	adbc := adb.New(os.Getenv("ADB_BIN"))

	// 补池放后台：Ensure 要在真实节点上建容器、等 boot，每台十几秒，
	// 放在监听之前会让 HTTP 端口迟迟不开，systemd 探活直接判死。
	// 顺序：先对账清孤儿（守护进程重启、基线测试残留都会留下占端口的容器），
	// 再造 golden（base 空着 overlay 挂不起来），最后补齐。
	if *ensure {
		go func() {
			ports := make([]int, 0, nc.MaxDevices)
			keep := map[string]bool{}
			for i := 1; i <= nc.MaxDevices; i++ {
				ports = append(ports, nc.PortRange[0]-1+i)
				keep[node.ContainerName(mgr.DeviceID(i))] = true
			}
			if removed, err := nd.Reconcile(ctx, keep, ports); err != nil {
				log.Warn("对账节点容器失败", "err", err)
			} else if len(removed) > 0 {
				log.Info("清理孤儿/占端口容器", "removed", removed)
			}
			goldenPort := nc.PortRange[1] // 用区间最后一个端口，不与设备端口重叠
			if err := nd.MakeGolden(ctx, nc.DataRoot+"/base", goldenPort); err != nil {
				log.Error("生成 golden 失败，设备将以空 base 启动", "err", err)
			} else {
				log.Info("golden 就绪", "base", nc.DataRoot+"/base")
			}
			// 库与节点对账：清掉「库里活跃但节点没容器」和「卡在中间态」的脏记录
			if running, err := nd.RunningSet(ctx); err != nil {
				log.Warn("读节点容器列表失败，跳过库对账", "err", err)
			} else {
				mgr.ReconcileStore(ctx, running)
			}
			log.Info("补齐设备池", "node", nc.Name, "max_devices", nc.MaxDevices)
			if err := mgr.Ensure(ctx); err != nil {
				log.Error("补齐设备池失败", "err", err)
			}
			// 补齐后把设备 adb connect 上，设备墙才有画面
			if ds, err := st.ListDevices(); err == nil {
				for _, d := range ds {
					if err := adbc.Connect(ctx, d.ADBAddr); err != nil {
						log.Warn("adb 连接失败", "device", d.ID, "err", err)
					}
				}
			}
		}()
	}

	hub := api.NewHub()
	reaper := &pool.Reaper{
		Store: st, Resetter: mgr,
		OnReap: func(leaseID, deviceID string, reason pool.ReapReason) {
			hub.Publish("reap", map[string]any{"lease": leaseID, "device": deviceID, "reason": string(reason)})
		},
		Interval:    cfg.ReapInterval.Duration,
		IdleTimeout: cfg.IdleTimeout.Duration,
		MaxLifetime: cfg.MaxLifetime.Duration,
		Log:         log,
	}
	log.Info("watchdog 启动", "巡检间隔", cfg.ReapInterval.Duration,
		"空闲超时", cfg.IdleTimeout.Duration, "生命周期上限", cfg.MaxLifetime.Duration)
	go reaper.Run(ctx)

	// 健康检查：连续 3 次 adb 探活失败即标 broken 并重建。
	// 没有它，一台容器挂了池子不知道，会一直把它分给人。
	hc := &pool.HealthChecker{Store: st, Prober: adbc, Resetter: mgr, Interval: 30 * time.Second, Log: log}
	go hc.Run(ctx)

	srv := &api.Server{
		Store: st, Token: cfg.Token,
		DefaultTTL: cfg.DefaultTTL.Duration, MaxTTL: cfg.MaxTTL.Duration,
		MinAvailMiB: cfg.MinAvailMiB,
		Health:      healthAdapter{nd},
		Log:         log,
		Screen:      adbc,
		Events:      hub,
		Resetter:    mgr,
		Scrcpy: api.ScrcpyConfig{
			// 未设置时 H.264 端点返回 503，前端自动退回截图流
			ServerJar: os.Getenv("SCRCPY_SERVER_JAR"),
			PortBase:  27200,
			MaxFPS:    15,
			BitRate:   4_000_000,
		},
	}
	h := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.Shutdown(sc)
	}()

	log.Info("droidpoold 启动", "listen", cfg.Listen, "node", nc.Name)
	if err := h.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("已停止")
	return nil
}

// healthAdapter 把 node.Node 的 Health 适配成 api.NodeHealth（不带 ctx）。
type healthAdapter struct{ n *node.Node }

func (a healthAdapter) Health() (*node.Health, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return a.n.Health(ctx)
}
