// Package certfile 从文件加载 TLS 证书，并在文件被换掉后自动用新的。
//
// 证书由 acme.sh 在别的机器上续签后推过来，droidpoold 不该为此重启：
// 重启会断掉所有设备墙的会话，而续签是每两个月一次的例行事。
// 做法很朴素：每次握手最多每 checkEvery 去 stat 一次证书文件，
// mtime 变了就重新读；读失败（推到一半、文件损坏）就继续用手里的旧证书。
package certfile

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// Loader 实现 tls.Config.GetCertificate。
type Loader struct {
	certPath, keyPath string
	// CheckEvery 两次 stat 之间的最短间隔，零值取 30 秒。
	CheckEvery time.Duration
	Log        *slog.Logger

	mu        sync.Mutex
	cert      *tls.Certificate
	mtime     time.Time
	checkedAt time.Time
}

// New 立即加载一次，路径错、文件坏在启动时就报出来，而不是等到第一次握手。
func New(certPath, keyPath string) (*Loader, error) {
	l := &Loader{certPath: certPath, keyPath: keyPath}
	cert, mtime, err := l.load()
	if err != nil {
		return nil, err
	}
	l.cert, l.mtime, l.checkedAt = cert, mtime, time.Now()
	return l, nil
}

func (l *Loader) load() (*tls.Certificate, time.Time, error) {
	st, err := os.Stat(l.certPath)
	if err != nil {
		return nil, time.Time{}, err
	}
	cert, err := tls.LoadX509KeyPair(l.certPath, l.keyPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("加载证书 %s / %s: %w", l.certPath, l.keyPath, err)
	}
	return &cert, st.ModTime(), nil
}

// GetCertificate 给 tls.Config 用；忽略 ClientHello，单证书。
func (l *Loader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return l.Current(), nil
}

// Current 返回当前证书，顺带按节奏检查文件是否换新。
func (l *Loader) Current() *tls.Certificate {
	l.mu.Lock()
	defer l.mu.Unlock()
	every := l.CheckEvery
	if every == 0 {
		every = 30 * time.Second
	}
	if time.Since(l.checkedAt) < every {
		return l.cert
	}
	l.checkedAt = time.Now()
	st, err := os.Stat(l.certPath)
	if err != nil || st.ModTime().Equal(l.mtime) {
		return l.cert
	}
	cert, mtime, err := l.load()
	if err != nil {
		// 推送可能只写了一半；留着旧证书继续服务，下次再试
		if l.Log != nil {
			l.Log.Warn("证书文件变了但加载失败，继续用旧的", "err", err)
		}
		return l.cert
	}
	l.cert, l.mtime = cert, mtime
	if l.Log != nil {
		l.Log.Info("已换用新证书", "cert", l.certPath, "mtime", mtime)
	}
	return l.cert
}
