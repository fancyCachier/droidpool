package node

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
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

// camConfigPath 每台设备自己那份 external camera 配置在宿主上的位置。
func (n *Node) camConfigPath(deviceID string) string {
	return n.DataRoot + "/camcfg/" + deviceID + ".xml"
}

// WriteCameraConfig 给这台设备生成一份只认它自己那个 v4l2 节点的 HAL 配置。
//
// 为什么非做不可：--device 在这里**不构成隔离**。redroid 必须 --privileged
// （binder 要求），而特权容器直接看得到宿主整个 /dev——实测每台设备都能列出
// 全部 8 个 /dev/video*。而 external camera HAL 会把它看到的每个 /dev/video*
// 都当成一个摄像头。没推流的节点因为拿不到格式会被 HAL 自己丢掉，所以平时
// 看不出问题；一旦有别的设备在推流，那一路就会出现在**每台**设备的相机列表里
// ——设备 1 能读到设备 3 的画面。
//
// HAL 的配置路径写死在 /vendor/etc/external_camera_config.xml（AOSP 的
// kDefaultCfgPath），但那是容器内的路径，按设备挂一份进去就能覆盖掉镜像里
// 那份公共的。<ignore> 段收的是设备号，把不属于自己的号全列进去。
func (n *Node) WriteCameraConfig(ctx context.Context, deviceID string) error {
	if n.CameraVideoBase == 0 {
		return nil
	}
	mine := n.CameraVideoBase + trailingNumber(deviceID)
	var ignores strings.Builder
	// 覆盖整个号段：不知道节点上到底建了几个，多列几个无害
	for i := n.CameraVideoBase; i <= n.CameraVideoBase+camMaxDevices; i++ {
		if i != mine {
			fmt.Fprintf(&ignores, "<id>%d</id>", i)
		}
	}
	cfg := fmt.Sprintf(camConfigTmpl, ignores.String())
	dir := n.DataRoot + "/camcfg"
	// 经一次性容器写，不依赖节点的 sudo——与 WipeData 同一条权限通道
	_, err := n.docker(ctx, "run", "--rm", "-v", dir+":/out", "busybox:stable",
		"sh", "-c", "cat > /out/"+deviceID+".xml <<'DROIDPOOLEOF'\n"+cfg+"\nDROIDPOOLEOF")
	if err != nil {
		return fmt.Errorf("写 %s 的摄像头配置: %w", deviceID, err)
	}
	return nil
}

// camMaxDevices 号段宽度，够覆盖 max_devices。
const camMaxDevices = 16

// camConfigTmpl 与 device/redroid-patches/external_camera_config.xml 保持一致，
// 只是 <ignore> 由每台设备各自填。fpsBound 给到 30 的理由见那个文件的注释。
const camConfigTmpl = `<ExternalCamera>
    <Provider>
        <ignore>%s</ignore>
    </Provider>
    <Device>
        <MaxJpegBufferSize bytes="3145728"/>
        <NumVideoBuffers count="4"/>
        <NumStillBuffers count="2"/>
        <FpsList>
            <Limit width="640" height="480" fpsBound="30.0"/>
            <Limit width="1280" height="720" fpsBound="30.0"/>
        </FpsList>
    </Device>
</ExternalCamera>`

// settle 返回等推流稳定的时长；测试里可缩短。
func (n *Node) settle() time.Duration {
	if n.camSettle > 0 {
		return n.camSettle
	}
	return camFeedSettle
}

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
	if _, err := n.docker(ctx, "run", "-d", "--name", name,
		"--restart", "unless-stopped", "--device", dev,
		camFeedImage,
		"-hide_banner", "-loglevel", "warning", "-nostdin",
		"-rtsp_transport", "tcp", "-re", "-i", rtsp,
		"-vf", "scale=1280:720", "-r", strconv.Itoa(camFPS),
		"-c:v", "mjpeg", "-q:v", "5", "-f", "v4l2", dev); err != nil {
		return err
	}
	return n.rescanCamera(ctx, deviceID)
}

// rescanCamera 让设备侧的 camera HAL 重新扫一遍 /dev/video*。
//
// 不做这一步的话，先起的设备永远认不出后接的摄像头：exclusive_caps 的
// v4l2loopback 节点在**没有 writer 时不暴露 VIDEO_CAPTURE**，而 HAL 只在
// 启动时扫一次——设备重建时推流还没起，HAL 扫到一个「不支持 VIDEO_CAPTURE」
// 的节点就永久丢弃了，之后再推流它也不会回头看。日志里是这一行：
//
//	W ExtCamPrvdr: deviceAdded device /dev/video23 does not support VIDEO_CAPTURE
//
// 重启整个容器也能解决，但那会中断设备与租约；只重启这个 HAL 服务就够，
// 实测重启后立刻认出 1 个摄像头，设备本身无感。
func (n *Node) rescanCamera(ctx context.Context, deviceID string) error {
	// 推流刚起，等它把格式协商出来再让 HAL 扫，否则扫了还是「不支持 CAPTURE」
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(n.settle()):
	}
	_, err := n.docker(ctx, "exec", ContainerName(deviceID),
		"sh", "-c", "setprop ctl.restart "+camHALService)
	return err
}

const (
	// camHALService 设备侧摄像头 HAL 的 init 服务名，与 AOSP 的
	// android.hardware.camera.provider-V1-external-service.rc 一致。
	camHALService = "vendor.camera.provider-ext"
	// camFeedSettle 等推流把 v4l2 节点的格式协商出来。实测几秒即可，
	// 给足余量——这段等待只在换源时发生，不在热路径上。
	camFeedSettle = 8 * time.Second
)

// removeCamFeed 清掉推流容器，设备销毁时调用。
func (n *Node) removeCamFeed(ctx context.Context, deviceID string) {
	_, _ = n.docker(ctx, "rm", "-f", CamFeedName(deviceID))
}
