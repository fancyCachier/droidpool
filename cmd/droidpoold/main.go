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

	if *ensure {
		log.Info("补齐设备池", "node", nc.Name, "max_devices", nc.MaxDevices)
		if err := mgr.Ensure(ctx); err != nil {
			return fmt.Errorf("补齐设备池: %w", err)
		}
	}

	reaper := &pool.Reaper{Store: st, Resetter: mgr, Interval: time.Minute, Log: log}
	go reaper.Run(ctx)

	srv := &api.Server{
		Store: st, Token: cfg.Token,
		DefaultTTL: cfg.DefaultTTL.Duration, MaxTTL: cfg.MaxTTL.Duration,
		SwapGuardMiB: cfg.SwapGuardMiB,
		Health:       healthAdapter{nd},
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
