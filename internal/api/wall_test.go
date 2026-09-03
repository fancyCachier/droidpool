package api

import "testing"

func TestChecksumDistinguishesFrames(t *testing.T) {
	a := make([]byte, 4096)
	b := make([]byte, 4096)
	for i := range a {
		a[i] = byte(i)
		b[i] = byte(i)
	}
	if checksum(a) != checksum(b) {
		t.Error("相同内容应得到相同指纹，否则去重完全失效")
	}
	// 改一个抽样点必须被发现
	b[0] = 0xFF
	if checksum(a) == checksum(b) {
		t.Error("首字节变化未被发现")
	}
	// 长度变化必须被发现（JPEG 内容一变长度通常就变）
	if checksum(a) == checksum(a[:4000]) {
		t.Error("长度变化未被发现")
	}
	// 空输入不该 panic
	checksum(nil)
	checksum([]byte{})
}

// 任意单字节变化都必须被发现：漏判会把「画面变了」当成「没变」而跳帧，
// 界面直接冻住。抽样式指纹过不了这一条，所以必须全量哈希。
func TestChecksumCatchesAnySingleByteChange(t *testing.T) {
	base := make([]byte, 100000)
	for i := range base {
		base[i] = byte(i * 7)
	}
	h := checksum(base)
	// 覆盖头、中、尾以及若干随机位置
	for _, pos := range []int{0, 1, 999, 50000, 73331, len(base) - 2, len(base) - 1} {
		mod := make([]byte, len(base))
		copy(mod, base)
		mod[pos] ^= 0xFF
		if checksum(mod) == h {
			t.Errorf("第 %d 字节的变化未被发现，该帧会被误判为静止而跳过", pos)
		}
	}
}
