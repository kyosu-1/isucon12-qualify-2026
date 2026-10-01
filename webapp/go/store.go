package isuports

// テナントデータのインメモリストア。SQLite は永続化用の write-through ストアとしてだけ使い、
// 読み取りは全てメモリから返す。排他は flock ではなくテナント単位の RWMutex。

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

// JSON の文字列リテラル（引用符込み）にしておく。レスポンスは手書きで組み立てる
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

const tenantSchemaV2 = `
CREATE TABLE IF NOT EXISTS competition (
  id VARCHAR(255) NOT NULL PRIMARY KEY,
  tenant_id BIGINT NOT NULL,
  title TEXT NOT NULL,
  finished_at BIGINT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS player (
  id VARCHAR(255) NOT NULL PRIMARY KEY,
  tenant_id BIGINT NOT NULL,
  display_name TEXT NOT NULL,
  is_disqualified BOOLEAN NOT NULL,
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS player_score (
  competition_id VARCHAR(255) NOT NULL,
  player_id VARCHAR(255) NOT NULL,
  score BIGINT NOT NULL,
  row_num BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS visit_history (
  competition_id VARCHAR(255) NOT NULL,
  player_id VARCHAR(255) NOT NULL,
  created_at BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS billing_report (
  competition_id VARCHAR(255) NOT NULL PRIMARY KEY,
  player_count BIGINT NOT NULL,
  visitor_count BIGINT NOT NULL
);
CREATE INDEX IF NOT EXISTS player_score_comp ON player_score (competition_id);
CREATE INDEX IF NOT EXISTS visit_history_comp ON visit_history (competition_id);
`

type playerT struct {
	ID          string
	DisplayName string
	Disq        bool
	CreatedAt   int64
	idJSON      string
	nameJSON    string
}

func newPlayer(id, name string, disq bool, createdAt int64) *playerT {
	return &playerT{ID: id, DisplayName: name, Disq: disq, CreatedAt: createdAt, idJSON: jsonStr(id), nameJSON: jsonStr(name)}
}

type scoreT struct {
	Score  int64
	RowNum int64
}

type compT struct {
	ID         string
	Title      string
	Finished   bool
	FinishedAt int64
	CreatedAt  int64

	idJSON    string
	titleJSON string

	scores map[string]scoreT // player_id -> 最後に CSV に登場したスコア
	// ランキングはエントリごとに JSON 化済み（"{...}," の連結）。rankOff[i] が i 番目の先頭。score 入稿時に作り直す
	rankBuf []byte
	rankOff []int32

	visitMu  sync.Mutex
	visitors map[string]struct{} // 開催中に ranking を見た player_id

	playerCount  int64 // 終了時に確定
	visitorCount int64
}

type tenantT struct {
	once    sync.Once
	loadErr error
	reqs    int64 // 計測用: このテナントへのリクエスト数

	mu sync.RWMutex
	id int64
	db *sql.DB

	players    map[string]*playerT
	playerList []*playerT // created_at 昇順（同値は挿入順）
	playerDesc []*playerT // created_at 降順（同値は挿入順）。nil なら作り直す
	comps      map[string]*compT
	compList   []*compT
	compDesc   []*compT
}

var (
	tenantsMu sync.Mutex
	tenants   = map[int64]*tenantT{}
)

func tenantDBDir() string { return getEnv("ISUCON_TENANT_DB_DIR", "../tenant_db") }

func tenantDBPath(id int64) string {
	return filepath.Join(tenantDBDir(), fmt.Sprintf("%d.db", id))
}

func openTenantSQLite(p string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000", p))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// テナントを取得する。初回アクセス時に SQLite から読み込む（無ければ作る）
func getTenant(id int64) (*tenantT, error) {
	tenantsMu.Lock()
	t, ok := tenants[id]
	if !ok {
		t = &tenantT{id: id}
		tenants[id] = t
	}
	tenantsMu.Unlock()
	atomic.AddInt64(&t.reqs, 1)
	t.once.Do(func() { t.loadErr = t.load() })
	if t.loadErr != nil {
		return nil, t.loadErr
	}
	return t, nil
}

// 全テナントを閉じて忘れる（initialize 用）
func resetTenants() {
	tenantsMu.Lock()
	defer tenantsMu.Unlock()
	for _, t := range tenants {
		t.mu.Lock()
		if t.db != nil {
			t.db.Close()
		}
		t.mu.Unlock()
	}
	tenants = map[int64]*tenantT{}
}

func (t *tenantT) load() error {
	db, err := openTenantSQLite(tenantDBPath(t.id))
	if err != nil {
		return fmt.Errorf("open tenant db %d: %w", t.id, err)
	}
	if _, err := db.Exec(tenantSchemaV2); err != nil {
		return fmt.Errorf("create schema tenant %d: %w", t.id, err)
	}
	t.db = db
	t.players = map[string]*playerT{}
	t.comps = map[string]*compT{}

	rows, err := db.Query("SELECT id, display_name, is_disqualified, created_at FROM player ORDER BY created_at, rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		p := &playerT{}
		if err := rows.Scan(&p.ID, &p.DisplayName, &p.Disq, &p.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		p.idJSON, p.nameJSON = jsonStr(p.ID), jsonStr(p.DisplayName)
		t.players[p.ID] = p
		t.playerList = append(t.playerList, p)
	}
	rows.Close()

	rows, err = db.Query("SELECT id, title, finished_at, created_at FROM competition ORDER BY created_at, rowid")
	if err != nil {
		return err
	}
	for rows.Next() {
		c := &compT{scores: map[string]scoreT{}, visitors: map[string]struct{}{}}
		var fin sql.NullInt64
		if err := rows.Scan(&c.ID, &c.Title, &fin, &c.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		c.Finished, c.FinishedAt = fin.Valid, fin.Int64
		c.idJSON, c.titleJSON = jsonStr(c.ID), jsonStr(c.Title)
		t.comps[c.ID] = c
		t.compList = append(t.compList, c)
	}
	rows.Close()

	rows, err = db.Query("SELECT competition_id, player_id, score, row_num FROM player_score")
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid, pid string
		var s scoreT
		if err := rows.Scan(&cid, &pid, &s.Score, &s.RowNum); err != nil {
			rows.Close()
			return err
		}
		if c, ok := t.comps[cid]; ok {
			if old, ok := c.scores[pid]; !ok || old.RowNum < s.RowNum {
				c.scores[pid] = s
			}
		}
	}
	rows.Close()

	rows, err = db.Query("SELECT competition_id, player_id FROM visit_history")
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid, pid string
		if err := rows.Scan(&cid, &pid); err != nil {
			rows.Close()
			return err
		}
		if c, ok := t.comps[cid]; ok && !c.Finished {
			c.visitors[pid] = struct{}{}
		}
	}
	rows.Close()

	rows, err = db.Query("SELECT competition_id, player_count, visitor_count FROM billing_report")
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid string
		var pc, vc int64
		if err := rows.Scan(&cid, &pc, &vc); err != nil {
			rows.Close()
			return err
		}
		if c, ok := t.comps[cid]; ok {
			c.playerCount, c.visitorCount = pc, vc
		}
	}
	rows.Close()

	for _, c := range t.compList {
		t.rebuildRanks(c)
	}
	return nil
}

// ランキングを作り直す（score 降順、同点は row_num 昇順）
func (t *tenantT) rebuildRanks(c *compT) {
	type rk struct {
		score, rowNum int64
		p             *playerT
		pid           string
	}
	ranks := make([]rk, 0, len(c.scores))
	for pid, s := range c.scores {
		ranks = append(ranks, rk{score: s.Score, rowNum: s.RowNum, p: t.players[pid], pid: pid})
	}
	sort.Slice(ranks, func(i, j int) bool {
		if ranks[i].score == ranks[j].score {
			return ranks[i].rowNum < ranks[j].rowNum
		}
		return ranks[i].score > ranks[j].score
	})
	buf := make([]byte, 0, len(ranks)*96)
	off := make([]int32, 0, len(ranks)+1)
	for i, r := range ranks {
		off = append(off, int32(len(buf)))
		buf = append(buf, `{"rank":`...)
		buf = strconv.AppendInt(buf, int64(i+1), 10)
		buf = append(buf, `,"score":`...)
		buf = strconv.AppendInt(buf, r.score, 10)
		buf = append(buf, `,"player_id":`...)
		if r.p != nil {
			buf = append(buf, r.p.idJSON...)
			buf = append(buf, `,"player_display_name":`...)
			buf = append(buf, r.p.nameJSON...)
		} else {
			buf = append(buf, jsonStr(r.pid)...)
			buf = append(buf, `,"player_display_name":""`...)
		}
		buf = append(buf, '}', ',')
	}
	off = append(off, int32(len(buf)))
	c.rankBuf, c.rankOff = buf, off
}

func (t *tenantT) compsDesc() []*compT {
	if d := t.compDesc; d != nil {
		return d
	}
	d := make([]*compT, len(t.compList))
	copy(d, t.compList)
	sort.SliceStable(d, func(i, j int) bool { return d[i].CreatedAt > d[j].CreatedAt })
	t.compDesc = d
	return d
}

func (t *tenantT) playersDesc() []*playerT {
	if d := t.playerDesc; d != nil {
		return d
	}
	d := make([]*playerT, len(t.playerList))
	copy(d, t.playerList)
	sort.SliceStable(d, func(i, j int) bool { return d[i].CreatedAt > d[j].CreatedAt })
	t.playerDesc = d
	return d
}

func (c *compT) report() BillingReport {
	r := BillingReport{CompetitionID: c.ID, CompetitionTitle: c.Title}
	if c.Finished {
		r.PlayerCount = c.playerCount
		r.VisitorCount = c.visitorCount
		r.BillingPlayerYen = 100 * c.playerCount
		r.BillingVisitorYen = 10 * c.visitorCount
		r.BillingYen = r.BillingPlayerYen + r.BillingVisitorYen
	}
	return r
}

// initial_data_v2 を tenant_db にコピーする（initialize 用）
func restoreTenantDBs() error {
	dir := tenantDBDir()
	old, _ := filepath.Glob(filepath.Join(dir, "*.db*"))
	for _, f := range old {
		os.Remove(f)
	}
	src := getEnv("ISUCON_INITIAL_DATA_DIR", "../../initial_data_v2")
	files, err := filepath.Glob(filepath.Join(src, "*.db"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no initial data in %s (run: isuports migrate)", src)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.Base(f)), b, 0644); err != nil {
			return err
		}
	}
	return nil
}
