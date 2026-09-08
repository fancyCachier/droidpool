package node

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/fancyCachier/droidpool/internal/pool"
)

// 硬件身份与 mock 定位。
//
// 身份（Build.MODEL 等）来自镜像里四个分区的 build.prop，都是 ro.* 属性，
// 开机定死。做法是把这四个文件从镜像里拷出来、改掉 ro.product.<分区>.* 那几行，
// 再以只读 bind mount 覆盖回容器里对应的路径（2026-09-09 实测：只改 system
// 与 vendor 不生效，ro.product.model 按 product → odm → vendor → system_ext
// → system 取第一个非空值，product 分区那份会赢）。序列号走启动参数
// androidboot.serialno，init 会导出成 ro.serialno。
//
// 定位是运行态的：Android 自带的 test provider，用 shell 用户（uid 2000）
// 通过 `cmd location` 注入。docker exec 默认是 root，而 AppOps 按调用方
// uid 判 MOCK_LOCATION，root 会被拒（实测 "android from uid 0 not allowed"），
// 所以必须 -u 2000。test provider 不落盘，容器一重建就没了，由 Manager 重放。

// propFiles 要覆盖的四份属性文件：容器内路径 → 宿主上的文件名。
// product / system_ext 在镜像里是 /system 下的目录（/product 只是符号链接），
// 挂载目标写实际路径，不依赖 docker 解析容器内的符号链接。
var propFiles = []struct{ inContainer, name string }{
	{"/system/build.prop", "system.prop"},
	{"/vendor/build.prop", "vendor.prop"},
	{"/system/product/etc/build.prop", "product.prop"},
	{"/system/system_ext/etc/build.prop", "system_ext.prop"},
}

// propsDir 某台设备（或 golden）的属性文件在宿主上的目录。
func (n *Node) propsDir(name string) string { return n.DataRoot + "/props/" + name }

// DefaultSerial 身份没给序列号时按设备 id 派生：DP + 大写字母数字。
// 每台不同即可，应用拿它当设备标识时不会撞。
func DefaultSerial(deviceID string) string {
	var b strings.Builder
	b.WriteString("DP")
	for _, r := range strings.ToUpper(deviceID) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// identityArgs 为一台设备准备身份：写好四份属性文件，返回要追加到
// docker run 的挂载参数与启动参数。ident 为 nil 时两者皆空——镜像原样。
//
// 原文件从镜像里拿：create 一个不启动的容器（redroid 没有 init 跑不了 sh，
// 只能这样取文件），`docker cp … -` 以 tar 流送回控制面，在这里改写后经一次性
// busybox 容器写到节点上——与 WriteCameraConfig 同一条权限通道。不用
// `docker cp` 直接落宿主路径：那一步是 docker **客户端**以自己的身份写文件，
// 目录是 root 建的就 permission denied（2026-09-09 实测）。
func (n *Node) identityArgs(ctx context.Context, name string, ident *pool.Identity) (mounts, bootArgs []string, err error) {
	if ident == nil {
		return nil, nil, nil
	}
	if err := ident.Validate(); err != nil {
		return nil, nil, err
	}
	tmp := "droidpool-props-" + name
	_, _ = n.docker(ctx, "rm", "-f", tmp)
	if _, err := n.docker(ctx, "create", "--name", tmp, n.Image); err != nil {
		return nil, nil, fmt.Errorf("从镜像取 build.prop: %w", err)
	}
	defer n.docker(context.Background(), "rm", "-f", tmp)
	var script strings.Builder
	fmt.Fprintf(&script, "mkdir -p /out/%s", name)
	for _, f := range propFiles {
		out, err := n.docker(ctx, "cp", tmp+":"+f.inContainer, "-")
		if err != nil {
			return nil, nil, fmt.Errorf("拷 %s: %w", f.inContainer, err)
		}
		content, err := firstFileInTar(out)
		if err != nil {
			return nil, nil, fmt.Errorf("解析 %s: %w", f.inContainer, err)
		}
		fmt.Fprintf(&script, " && cat > /out/%s/%s <<'DROIDPOOLEOF'\n%sDROIDPOOLEOF\n",
			name, f.name, rewriteProps(content, *ident))
	}
	if _, err := n.docker(ctx, "run", "--rm", "-v", n.DataRoot+"/props:/out", "busybox:stable",
		"sh", "-c", script.String()); err != nil {
		return nil, nil, fmt.Errorf("写 %s 的属性文件: %w", name, err)
	}
	dir := n.propsDir(name)
	for _, f := range propFiles {
		mounts = append(mounts, "-v", dir+"/"+f.name+":"+f.inContainer+":ro")
	}
	serial := ident.Serial
	if serial == "" {
		serial = DefaultSerial(name)
	}
	return mounts, []string{"androidboot.serialno=" + serial}, nil
}

// firstFileInTar 取 `docker cp … -` 输出的 tar 流里第一个普通文件的内容。
func firstFileInTar(stream string) (string, error) {
	tr := tar.NewReader(strings.NewReader(stream))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("tar 流里没有文件")
		}
		if err != nil {
			return "", err
		}
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA {
			b, err := io.ReadAll(tr)
			return string(b), err
		}
	}
}

// identityKeys ro.product.<分区>.<键> 里要改写的键。
var identityKeys = map[string]func(pool.Identity) string{
	"model":        func(id pool.Identity) string { return id.Model },
	"brand":        func(id pool.Identity) string { return id.Brand },
	"manufacturer": func(id pool.Identity) string { return id.Manufacturer },
	"device":       func(id pool.Identity) string { return id.Device },
	"name":         func(id pool.Identity) string { return id.Name },
}

// rewriteProps 改写一份 build.prop 里的 ro.product.<分区>.{model,brand,
// manufacturer,device,name}，其余行原样保留。四个分区的文件键名只差分区段，
// 一个函数通吃。输出保证以换行结尾，好放进 heredoc。
func rewriteProps(content string, id pool.Identity) string {
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	for i, line := range lines {
		key, _, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(key, "ro.product.") {
			continue
		}
		parts := strings.Split(key, ".") // ro product <分区> <键>
		if len(parts) != 4 {
			continue
		}
		if get, ok := identityKeys[parts[3]]; ok {
			lines[i] = key + "=" + get(id)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// SetLocation 给运行中的设备设 mock 定位（"纬度,经度"），空串撤销。
//
// gps 与 network 两个 provider 都设：镜像里没有 GNSS HAL，本来一个真 provider
// 都没有，fused provider 是从这两个里取的，只设一个的话走 fused 的应用可能拿不到。
// 每次先删再加，改坐标时不用区分是首次还是更新。
func (n *Node) SetLocation(ctx context.Context, deviceID, location string) error {
	lat, lng, err := pool.ParseLocation(location)
	if err != nil {
		return err
	}
	var script string
	if location == "" {
		script = "for p in gps network; do cmd location providers remove-test-provider $p >/dev/null 2>&1; done; true"
	} else {
		loc := pool.FormatLocation(lat, lng)
		script = "appops set com.android.shell android:mock_location allow && " +
			"for p in gps network; do " +
			"cmd location providers remove-test-provider $p >/dev/null 2>&1; " +
			"cmd location providers add-test-provider $p && " +
			"cmd location providers set-test-provider-enabled $p true && " +
			"cmd location providers set-test-provider-location $p --location " + loc + " || exit 1; " +
			"done"
	}
	_, err = n.docker(ctx, "exec", "-u", "2000", ContainerName(deviceID), "sh", "-c", script)
	if err != nil {
		return fmt.Errorf("设 mock 定位: %w", err)
	}
	return nil
}
