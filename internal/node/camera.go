package node

import (
	"context"
	"fmt"
	"strconv"
)

// 摄像头画面链路：宿主上每台设备一个 v4l2loopback 节点，一个 ffmpeg 容器把
// RTSP 转成 MJPEG 灌进去，redroid 以 --device 拿到该节点，镜像里的
// external camera HAL 把它当成普通 UVC 摄像头。
//
//	droidpool-cam-<id>   ffmpeg，RTSP → /dev/videoN   ← 换源只重建它
//	/dev/videoN          v4l2loopback（setup-node.sh 建）
//	droidpool-<id>       redroid，--device /dev/videoN
//
// 和出口那套是同一个形状：设备节点在容器生命周期内固定，背后的内容随时可换。
// 换源重建的是推流容器，设备本身不动，租约不中断。
const (
	// camFeedImage 只用它跑 ffmpeg。选带 ffmpeg 的现成镜像而不是自己做，
	// 是因为这里不需要额外工具——set-fps 得在宿主做（见 SetCamera 注释）。
	camFeedImage = "linuxserver/ffmpeg"
	// camFPS 推流帧率。必须与 external_camera_config.xml 里对应分辨率的
	// fpsBound 一致或更低：设备报的 fps 超过配置上限时 HAL 会把整个设备丢掉，
	// 相机数恒为 0，而错误信息只说 characteristics 失败，看不出是帧率的事。
	camFPS = 15
)

// CamFeedName 某台设备的推流容器名。
func CamFeedName(deviceID string) string { return "droidpool-cam-" + deviceID }

// SetCamera 换这台设备的画面源。rtsp 为空表示停流（相机随之报 0 个设备）。
//
// 只重建推流容器，redroid 与它的 /dev/videoN 绑定都不动。
//
// 注意 fps 是在宿主侧由 setup-node.sh 的 modprobe 参数与 v4l2loopback-ctl
// 决定的，不在这里设：v4l2loopback-ctl 是对设备 fd 做 ioctl，推流镜像里没有
// 这个工具，单为它做个镜像不划算。所以这里只保证编码帧率是 camFPS。
func (n *Node) SetCamera(ctx context.Context, deviceID, rtsp string) error {
	name := CamFeedName(deviceID)
	_, _ = n.docker(ctx, "rm", "-f", name) // 忽略「不存在」
	if rtsp == "" {
		return nil
	}
	dev := n.CameraDevice(deviceID)
	if dev == "" {
		return fmt.Errorf("节点未启用摄像头（config 的 camera_video_base 为 0）")
	}
	// -rtsp_transport tcp：UDP 丢包在容器里表现为花屏，排查成本高。
	// -c:v mjpeg：external camera HAL 只认 MJPEG 与 Z16，喂别的会被整个丢掉。
	// --restart unless-stopped：源抖动或对端重启时自己接回来，不然画面就永久黑了。
	_, err := n.docker(ctx, "run", "-d", "--name", name,
		"--restart", "unless-stopped", "--device", dev,
		camFeedImage,
		"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-rtsp_transport", "tcp", "-re", "-i", rtsp,
		"-vf", "scale=1280:720", "-r", strconv.Itoa(camFPS),
		"-c:v", "mjpeg", "-q:v", "5", "-f", "v4l2", dev)
	return err
}

// removeCamFeed 清掉推流容器，设备销毁时调用。
func (n *Node) removeCamFeed(ctx context.Context, deviceID string) {
	_, _ = n.docker(ctx, "rm", "-f", CamFeedName(deviceID))
}
