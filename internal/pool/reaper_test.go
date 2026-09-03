package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// fakeLeaseStore 只实现回收器需要的两个方法，不覆写业务逻辑。
type fakeLeaseStore struct {
	mu       sync.Mutex
	expired  []*Lease
	released []string
	failOn   map[string]error
}

func (f *fakeLeaseStore) ExpiredLeases(time.Time) ([]*Lease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.expired, nil
}

func (f *fakeLeaseStore) Release(id string, _ time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[id]; ok {
		return "", err
	}
	f.released = append(f.released, id)
	return "dev-" + id, nil
}

type fakeResetter struct {
	mu     sync.Mutex
	reset  []string
	failOn map[string]error
}

func (f *fakeResetter) Reset(_ context.Context, deviceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.failOn[deviceID]; ok {
		return err
	}
	f.reset = append(f.reset, deviceID)
	return nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReapOnceReleasesAndResets(t *testing.T) {
	fs := &fakeLeaseStore{expired: []*Lease{
		{ID: "L1", Owner: "woo@mac", Worktree: "wt-a"},
		{ID: "L2", Owner: "woo@linux", Worktree: "wt-b"},
	}}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}

	n, err := r.ReapOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("应回收 2 条，得到 %d", n)
	}
	if len(fs.released) != 2 {
		t.Errorf("两条租约都应被 release，得到 %v", fs.released)
	}
	// 回收后必须复位设备，否则脏设备直接回池给下一个 agent
	if len(fr.reset) != 2 || fr.reset[0] != "dev-L1" {
		t.Errorf("两台设备都应被复位，得到 %v", fr.reset)
	}
}

func TestReapOnceNothingExpired(t *testing.T) {
	fs := &fakeLeaseStore{}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}
	n, err := r.ReapOnce(context.Background())
	if err != nil || n != 0 {
		t.Errorf("无过期租约时应回收 0 条无错，得到 n=%d err=%v", n, err)
	}
	if len(fr.reset) != 0 {
		t.Errorf("不应触发任何复位，得到 %v", fr.reset)
	}
}

// 一条租约回收失败不能让整轮停摆——否则一台卡住的设备会拖垮整个池子。
func TestReapOnceContinuesAfterReleaseFailure(t *testing.T) {
	boom := errors.New("库锁住了")
	fs := &fakeLeaseStore{
		expired: []*Lease{{ID: "L1"}, {ID: "L2"}, {ID: "L3"}},
		failOn:  map[string]error{"L2": boom},
	}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}

	n, err := r.ReapOnce(context.Background())
	if err != nil {
		t.Fatalf("单条失败不应让整轮报错: %v", err)
	}
	if n != 2 {
		t.Errorf("应成功回收 2 条（跳过失败的 L2），得到 %d", n)
	}
	if len(fs.released) != 2 || fs.released[0] != "L1" || fs.released[1] != "L3" {
		t.Errorf("L1 与 L3 应被回收，得到 %v", fs.released)
	}
}

// 复位失败同样不能中断后续回收。
func TestReapOnceContinuesAfterResetFailure(t *testing.T) {
	fs := &fakeLeaseStore{expired: []*Lease{{ID: "L1"}, {ID: "L2"}}}
	fr := &fakeResetter{failOn: map[string]error{"dev-L1": errors.New("容器起不来")}}
	r := &Reaper{Store: fs, Resetter: fr, Log: quietLogger()}

	n, err := r.ReapOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("复位失败不影响回收计数，应为 2，得到 %d", n)
	}
	if len(fr.reset) != 1 || fr.reset[0] != "dev-L2" {
		t.Errorf("L2 的设备仍应被复位，得到 %v", fr.reset)
	}
}

func TestReapOnceWithoutResetter(t *testing.T) {
	fs := &fakeLeaseStore{expired: []*Lease{{ID: "L1"}}}
	r := &Reaper{Store: fs, Log: quietLogger()} // Resetter 为 nil
	n, err := r.ReapOnce(context.Background())
	if err != nil || n != 1 {
		t.Errorf("无 Resetter 时也应正常回收，得到 n=%d err=%v", n, err)
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	fs := &fakeLeaseStore{}
	r := &Reaper{Store: fs, Interval: time.Millisecond, Log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ctx 取消后 Run 应立即退出")
	}
}

func TestRunReapsOnTick(t *testing.T) {
	fs := &fakeLeaseStore{expired: []*Lease{{ID: "L1"}}}
	fr := &fakeResetter{}
	r := &Reaper{Store: fs, Resetter: fr, Interval: 5 * time.Millisecond, Log: quietLogger()}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)

	deadline := time.After(2 * time.Second)
	for {
		fs.mu.Lock()
		n := len(fs.released)
		fs.mu.Unlock()
		if n > 0 {
			return // 定时器确实触发了回收
		}
		select {
		case <-deadline:
			t.Fatal("Run 在 2s 内没有触发任何回收")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
