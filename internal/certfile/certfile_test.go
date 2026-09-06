package certfile

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePair 生成一张自签证书写到 dir，序列号用来区分新旧。
func writePair(t *testing.T, dir string, serial int64) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "droidpool.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "fullchain.pem")
	keyPath = filepath.Join(dir, "privkey.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func serialOf(t *testing.T, l *Loader) int64 {
	t.Helper()
	c, err := l.GetCertificate(nil)
	if err != nil || c == nil || c.Leaf == nil {
		t.Fatalf("取证书失败: %v", err)
	}
	return c.Leaf.SerialNumber.Int64()
}

func TestNewFailsOnMissingOrBrokenFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(filepath.Join(dir, "nope.pem"), filepath.Join(dir, "nope.key")); err == nil {
		t.Error("文件不存在应在启动时报错")
	}
	cp, kp := writePair(t, dir, 1)
	_ = os.WriteFile(kp, []byte("not a key"), 0o600)
	if _, err := New(cp, kp); err == nil {
		t.Error("私钥损坏应在启动时报错")
	}
}

// 证书文件换新后（mtime 变化）下一次握手就用新的，不用重启。
func TestReloadsWhenFileChanges(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, 1)
	l, err := New(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	l.CheckEvery = time.Nanosecond
	if got := serialOf(t, l); got != 1 {
		t.Fatalf("初始序列号 = %d", got)
	}
	writePair(t, dir, 2)
	// 同一秒内写入 mtime 可能相同，显式把 mtime 往后拨
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(cp, future, future)
	if got := serialOf(t, l); got != 2 {
		t.Errorf("文件换新后序列号 = %d，期望 2", got)
	}
}

// 推送推到一半文件不完整时，不能把正在服务的证书弄丢。
func TestKeepsOldCertWhenNewOneIsBroken(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, 1)
	l, err := New(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	l.CheckEvery = time.Nanosecond
	_ = os.WriteFile(cp, []byte("-----BEGIN CERTIFICATE-----\ngarbage\n-----END CERTIFICATE-----\n"), 0o600)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(cp, future, future)
	if got := serialOf(t, l); got != 1 {
		t.Errorf("新文件损坏时应继续用旧证书，得到序列号 %d", got)
	}
	// 修好之后要能换上
	writePair(t, dir, 3)
	later := time.Now().Add(4 * time.Second)
	_ = os.Chtimes(cp, later, later)
	if got := serialOf(t, l); got != 3 {
		t.Errorf("文件修好后应换新，得到序列号 %d", got)
	}
}

// 没到检查间隔就不 stat：mtime 变了也先用旧的，避免每次握手都打文件系统。
func TestRespectsCheckInterval(t *testing.T) {
	dir := t.TempDir()
	cp, kp := writePair(t, dir, 1)
	l, err := New(cp, kp)
	if err != nil {
		t.Fatal(err)
	}
	l.CheckEvery = time.Hour
	writePair(t, dir, 2)
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(cp, future, future)
	if got := serialOf(t, l); got != 1 {
		t.Errorf("检查间隔未到不应重载，得到序列号 %d", got)
	}
}
