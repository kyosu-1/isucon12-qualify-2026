package isuports

// 初期データの一回限りの変換: initial_data/*.db (元スキーマ) + MySQL visit_history
//   -> initial_data_v2/*.db (v2 スキーマ: player_score は最終行だけ、終了済み大会は billing_report に確定値)
//   -> MySQL tenant_billing_init (テナントごとの請求額合計)

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const adminExtraSchema = `
CREATE TABLE IF NOT EXISTS tenant_billing (
  tenant_id BIGINT NOT NULL PRIMARY KEY,
  billing BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARACTER SET=utf8mb4`

func Migrate() error {
	var err error
	adminDB, err = connectAdminDB()
	if err != nil {
		return err
	}
	defer adminDB.Close()
	for _, q := range []string{adminExtraSchema, strings.Replace(adminExtraSchema, "tenant_billing", "tenant_billing_init", 1)} {
		if _, err := adminDB.Exec(q); err != nil {
			return err
		}
	}
	srcDir := getEnv("ISUCON_ORIG_INITIAL_DATA_DIR", "../../initial_data")
	dstDir := getEnv("ISUCON_INITIAL_DATA_DIR", "../../initial_data_v2")
	if err := os.MkdirAll(dstDir, 0755); err != nil {
		return err
	}
	files, _ := filepath.Glob(filepath.Join(srcDir, "*.db"))
	for _, f := range files {
		id, err := strconv.ParseInt(strings.TrimSuffix(filepath.Base(f), ".db"), 10, 64)
		if err != nil {
			continue
		}
		total, err := migrateTenant(id, f, filepath.Join(dstDir, filepath.Base(f)))
		if err != nil {
			return fmt.Errorf("tenant %d: %w", id, err)
		}
		if _, err := adminDB.Exec("REPLACE INTO tenant_billing_init (tenant_id, billing) VALUES (?, ?)", id, total); err != nil {
			return err
		}
		fmt.Printf("tenant %d: billing=%d\n", id, total)
	}
	return nil
}

func migrateTenant(id int64, srcPath, dstPath string) (int64, error) {
	os.Remove(dstPath)
	src, err := sql.Open("sqlite3", "file:"+srcPath+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer src.Close()
	dst, err := sql.Open("sqlite3", "file:"+dstPath+"?_journal_mode=OFF&_synchronous=OFF")
	if err != nil {
		return 0, err
	}
	defer dst.Close()
	dst.SetMaxOpenConns(1)
	if _, err := dst.Exec(tenantSchemaV2); err != nil {
		return 0, err
	}
	tx, err := dst.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// player
	rows, err := src.Query("SELECT id, tenant_id, display_name, is_disqualified, created_at, updated_at FROM player ORDER BY rowid")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var pid, name string
		var tid, ca, ua int64
		var dq bool
		if err := rows.Scan(&pid, &tid, &name, &dq, &ca, &ua); err != nil {
			return 0, err
		}
		if _, err := tx.Exec("INSERT INTO player (id, tenant_id, display_name, is_disqualified, created_at, updated_at) VALUES (?,?,?,?,?,?)", pid, tid, name, dq, ca, ua); err != nil {
			return 0, err
		}
	}
	rows.Close()

	// competition
	type comp struct {
		finished   bool
		finishedAt int64
		scored     map[string]scoreT
		visitors   int64
	}
	comps := map[string]*comp{}
	rows, err = src.Query("SELECT id, tenant_id, title, finished_at, created_at, updated_at FROM competition ORDER BY rowid")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var cid, title string
		var tid, ca, ua int64
		var fin sql.NullInt64
		if err := rows.Scan(&cid, &tid, &title, &fin, &ca, &ua); err != nil {
			return 0, err
		}
		if _, err := tx.Exec("INSERT INTO competition (id, tenant_id, title, finished_at, created_at, updated_at) VALUES (?,?,?,?,?,?)", cid, tid, title, fin, ca, ua); err != nil {
			return 0, err
		}
		comps[cid] = &comp{finished: fin.Valid, finishedAt: fin.Int64, scored: map[string]scoreT{}}
	}
	rows.Close()

	// player_score: (competition, player) ごとに row_num 最大の行だけ残す
	rows, err = src.Query("SELECT competition_id, player_id, score, row_num FROM player_score")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var cid, pid string
		var s scoreT
		if err := rows.Scan(&cid, &pid, &s.Score, &s.RowNum); err != nil {
			return 0, err
		}
		c, ok := comps[cid]
		if !ok {
			continue
		}
		if old, ok := c.scored[pid]; !ok || old.RowNum < s.RowNum {
			c.scored[pid] = s
		}
	}
	rows.Close()
	ins, err := tx.Prepare("INSERT INTO player_score (competition_id, player_id, score, row_num) VALUES (?,?,?,?)")
	if err != nil {
		return 0, err
	}
	for cid, c := range comps {
		for pid, s := range c.scored {
			if _, err := ins.Exec(cid, pid, s.Score, s.RowNum); err != nil {
				return 0, err
			}
		}
	}
	ins.Close()

	// visit_history: 開催中の大会は行を持ち越す。終了済みは visitor 数に畳む
	vrows, err := adminDB.Query("SELECT competition_id, player_id, MIN(created_at) FROM visit_history WHERE tenant_id = ? GROUP BY competition_id, player_id", id)
	if err != nil {
		return 0, err
	}
	for vrows.Next() {
		var cid, pid string
		var minCreated int64
		if err := vrows.Scan(&cid, &pid, &minCreated); err != nil {
			return 0, err
		}
		c, ok := comps[cid]
		if !ok {
			continue
		}
		if !c.finished {
			if _, err := tx.Exec("INSERT INTO visit_history (competition_id, player_id, created_at) VALUES (?,?,?)", cid, pid, minCreated); err != nil {
				return 0, err
			}
			continue
		}
		if c.finishedAt < minCreated {
			continue
		}
		if _, scored := c.scored[pid]; !scored {
			c.visitors++
		}
	}
	vrows.Close()

	var total int64
	for cid, c := range comps {
		if !c.finished {
			continue
		}
		pc := int64(len(c.scored))
		if _, err := tx.Exec("INSERT INTO billing_report (competition_id, player_count, visitor_count) VALUES (?,?,?)", cid, pc, c.visitors); err != nil {
			return 0, err
		}
		total += 100*pc + 10*c.visitors
	}
	return total, tx.Commit()
}
