// Package store 用 SQLite 持久化设备与租约。
// 纯 Go 驱动（modernc.org/sqlite），与 fancyCashier Edge 同款，无需 CGO。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fancyCachier/droidpool/internal/pool"
)

var (
	ErrNotFound     = errors.New("不存在")
	ErrNoFreeDevice = errors.New("池中无空闲设备")
)

type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS devices (
  id            TEXT PRIMARY KEY,
  node          TEXT NOT NULL,
  container     TEXT NOT NULL,
  adb_addr      TEXT NOT NULL,
  state         TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  last_health_at INTEGER NOT NULL DEFAULT 0,
  health_fails  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS leases (
  id            TEXT PRIMARY KEY,
  device_id     TEXT NOT NULL REFERENCES devices(id),
  owner         TEXT NOT NULL,
  host          TEXT NOT NULL,
  worktree      TEXT NOT NULL,
  branch        TEXT NOT NULL DEFAULT '',
  head_sha      TEXT NOT NULL DEFAULT '',
  created_at    INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,
  human_takeover INTEGER NOT NULL DEFAULT 0,
  human_note    TEXT NOT NULL DEFAULT '',
  edge_mode     TEXT NOT NULL DEFAULT 'shared',
  released_at   INTEGER NOT NULL DEFAULT 0
);
-- 活跃租约里同一 (host, worktree) 只能有一条，保证 claim 幂等。
-- 已归还的行 released_at 非 0，不参与唯一约束。
CREATE UNIQUE INDEX IF NOT EXISTS idx_lease_idem
  ON leases(host, worktree) WHERE released_at = 0;
-- 一台设备同时只能有一个活跃租约。
CREATE UNIQUE INDEX IF NOT EXISTS idx_lease_device
  ON leases(device_id) WHERE released_at = 0;
`

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// SQLite 单写者，并发写会撞锁；池化到 1 条连接把写串行化。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0).UTC()
}

// ---------- 设备 ----------

func (s *Store) UpsertDevice(d *pool.Device) error {
	_, err := s.db.Exec(`
		INSERT INTO devices (id, node, container, adb_addr, state, created_at, last_health_at, health_fails)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
		  node=excluded.node, container=excluded.container, adb_addr=excluded.adb_addr,
		  state=excluded.state, last_health_at=excluded.last_health_at, health_fails=excluded.health_fails`,
		d.ID, d.Node, d.Container, d.ADBAddr, string(d.State),
		unix(d.CreatedAt), unix(d.LastHealthy), d.HealthFails)
	return err
}

func scanDevice(sc interface{ Scan(...any) error }) (*pool.Device, error) {
	var d pool.Device
	var st string
	var created, health int64
	if err := sc.Scan(&d.ID, &d.Node, &d.Container, &d.ADBAddr, &st, &created, &health, &d.HealthFails); err != nil {
		return nil, err
	}
	d.State = pool.DeviceState(st)
	d.CreatedAt = fromUnix(created)
	d.LastHealthy = fromUnix(health)
	return &d, nil
}

const deviceCols = `id, node, container, adb_addr, state, created_at, last_health_at, health_fails`

func (s *Store) GetDevice(id string) (*pool.Device, error) {
	row := s.db.QueryRow(`SELECT `+deviceCols+` FROM devices WHERE id=?`, id)
	d, err := scanDevice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return d, err
}

func (s *Store) ListDevices() ([]*pool.Device, error) {
	rows, err := s.db.Query(`SELECT ` + deviceCols + ` FROM devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pool.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SetDeviceState 校验状态机后落库。非法转移返回 pool.ErrBadTransition。
func (s *Store) SetDeviceState(id string, to pool.DeviceState) error {
	d, err := s.GetDevice(id)
	if err != nil {
		return err
	}
	if _, err := pool.Transition(d.State, to); err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE devices SET state=? WHERE id=?`, string(to), id)
	return err
}

// ---------- 租约 ----------

const leaseCols = `id, device_id, owner, host, worktree, branch, head_sha, created_at, expires_at, human_takeover, human_note, edge_mode`

func scanLease(sc interface{ Scan(...any) error }) (*pool.Lease, error) {
	var l pool.Lease
	var created, expires int64
	var takeover int
	var mode string
	if err := sc.Scan(&l.ID, &l.DeviceID, &l.Owner, &l.Host, &l.Worktree, &l.Branch, &l.HeadSHA,
		&created, &expires, &takeover, &l.HumanNote, &mode); err != nil {
		return nil, err
	}
	l.CreatedAt = fromUnix(created)
	l.ExpiresAt = fromUnix(expires)
	l.HumanTakeover = takeover != 0
	l.EdgeMode = pool.EdgeMode(mode)
	return &l, nil
}

// Claim 原子地取一台 ready 设备并建租约。
// 幂等：同一 (host, worktree) 已有活跃租约时直接返回它，不再占第二台设备。
// 池中无空闲设备时返回 ErrNoFreeDevice。
func (s *Store) Claim(l *pool.Lease, now time.Time) (*pool.Lease, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	// 幂等命中：复用既有租约
	row := tx.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE host=? AND worktree=? AND released_at=0`, l.Host, l.Worktree)
	if existing, err := scanLease(row); err == nil {
		return existing, true, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}

	// 取一台 ready 且没有活跃租约的设备
	var devID string
	err = tx.QueryRow(`
		SELECT d.id FROM devices d
		WHERE d.state=? AND NOT EXISTS (
		  SELECT 1 FROM leases l WHERE l.device_id=d.id AND l.released_at=0)
		ORDER BY d.id LIMIT 1`, string(pool.StateReady)).Scan(&devID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNoFreeDevice
	} else if err != nil {
		return nil, false, err
	}

	if _, err := tx.Exec(`UPDATE devices SET state=? WHERE id=?`, string(pool.StateLeased), devID); err != nil {
		return nil, false, err
	}
	l.DeviceID = devID
	if l.EdgeMode == "" {
		l.EdgeMode = pool.EdgeShared
	}
	if _, err := tx.Exec(`
		INSERT INTO leases (id, device_id, owner, host, worktree, branch, head_sha, created_at, expires_at, human_takeover, human_note, edge_mode)
		VALUES (?,?,?,?,?,?,?,?,?,0,'',?)`,
		l.ID, l.DeviceID, l.Owner, l.Host, l.Worktree, l.Branch, l.HeadSHA,
		unix(l.CreatedAt), unix(l.ExpiresAt), string(l.EdgeMode)); err != nil {
		return nil, false, err
	}
	return l, false, tx.Commit()
}

func (s *Store) GetLease(id string) (*pool.Lease, error) {
	row := s.db.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE id=? AND released_at=0`, id)
	l, err := scanLease(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return l, err
}

func (s *Store) ListLeases() ([]*pool.Lease, error) {
	rows, err := s.db.Query(`SELECT ` + leaseCols + ` FROM leases WHERE released_at=0 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pool.Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Renew 延长租约到 newExpiry。
func (s *Store) Renew(id string, newExpiry time.Time) error {
	res, err := s.db.Exec(`UPDATE leases SET expires_at=? WHERE id=? AND released_at=0`, unix(newExpiry), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetHumanTakeover 设置人工接管标志（设备墙上的接管/交还开关）。
func (s *Store) SetHumanTakeover(id string, on bool, note string) error {
	v := 0
	if on {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE leases SET human_takeover=?, human_note=? WHERE id=? AND released_at=0`, v, note, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Release 归还租约，设备转 resetting 等待复位。
func (s *Store) Release(id string, now time.Time) (deviceID string, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if err := tx.QueryRow(`SELECT device_id FROM leases WHERE id=? AND released_at=0`, id).Scan(&deviceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if _, err := tx.Exec(`UPDATE leases SET released_at=? WHERE id=?`, unix(now), id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE devices SET state=? WHERE id=?`, string(pool.StateResetting), deviceID); err != nil {
		return "", err
	}
	return deviceID, tx.Commit()
}

// ExpiredLeases 返回 now 时已过期的活跃租约。TTL 到期强制回收是唯一可靠的
// 回收路径——agent 异常退出是常态，不能指望它自己 release。
func (s *Store) ExpiredLeases(now time.Time) ([]*pool.Lease, error) {
	rows, err := s.db.Query(`SELECT `+leaseCols+` FROM leases WHERE released_at=0 AND expires_at<=?`, unix(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pool.Lease
	for rows.Next() {
		l, err := scanLease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
