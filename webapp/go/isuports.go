package isuports

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/labstack/gommon/log"
	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	_ "github.com/mattn/go-sqlite3"
)

const (
	cookieName = "isuports_session"

	RoleAdmin     = "admin"
	RoleOrganizer = "organizer"
	RolePlayer    = "player"
	RoleNone      = "none"
)

var (
	// 正しいテナント名の正規表現
	tenantNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

	adminDB *sqlx.DB

	jwtKey    interface{}
	baseHost  string
	adminHost string

	// ID 採番: 起動時刻(µs) からのカウンタ + ノード番号。再起動・複数ノードでも重複しない
	idCounter  int64
	nodeSuffix string

	tokenCache   sync.Map // token string -> *tokenClaims
	tenantByName sync.Map // tenant name -> *TenantRow
)

// 環境変数を取得する、なければデフォルト値を返す
func getEnv(key string, defaultValue string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultValue
}

// 管理用DBに接続する
func connectAdminDB() (*sqlx.DB, error) {
	config := mysql.NewConfig()
	config.Net = "tcp"
	config.Addr = getEnv("ISUCON_DB_HOST", "127.0.0.1") + ":" + getEnv("ISUCON_DB_PORT", "3306")
	config.User = getEnv("ISUCON_DB_USER", "isucon")
	config.Passwd = getEnv("ISUCON_DB_PASSWORD", "isucon")
	config.DBName = getEnv("ISUCON_DB_NAME", "isuports")
	config.ParseTime = true
	config.InterpolateParams = true
	dsn := config.FormatDSN()
	return sqlx.Open("mysql", dsn)
}

// システム全体で一意なIDを生成する
func dispenseID() string {
	return strconv.FormatInt(atomic.AddInt64(&idCounter, 1), 16) + nodeSuffix
}

// 全APIにCache-Control: privateを設定する
func SetCacheControlPrivate(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderCacheControl, "private")
		return next(c)
	}
}

// Run は cmd/isuports/main.go から呼ばれるエントリーポイントです
func Run() {
	e := echo.New()
	e.HideBanner = true
	e.Logger.SetLevel(log.ERROR)

	e.Use(middleware.Recover())
	e.Use(SetCacheControlPrivate)

	// SaaS管理者向けAPI
	e.POST("/api/admin/tenants/add", tenantsAddHandler)
	e.GET("/api/admin/tenants/billing", tenantsBillingHandler)

	// テナント管理者向けAPI - 参加者追加、一覧、失格
	e.GET("/api/organizer/players", playersListHandler)
	e.POST("/api/organizer/players/add", playersAddHandler)
	e.POST("/api/organizer/player/:player_id/disqualified", playerDisqualifiedHandler)

	// テナント管理者向けAPI - 大会管理
	e.POST("/api/organizer/competitions/add", competitionsAddHandler)
	e.POST("/api/organizer/competition/:competition_id/finish", competitionFinishHandler)
	e.POST("/api/organizer/competition/:competition_id/score", competitionScoreHandler)
	e.GET("/api/organizer/billing", billingHandler)
	e.GET("/api/organizer/competitions", organizerCompetitionsHandler)

	// 参加者向けAPI
	e.GET("/api/player/player/:player_id", playerHandler)
	e.GET("/api/player/competition/:competition_id/ranking", competitionRankingHandler)
	e.GET("/api/player/competitions", playerCompetitionsHandler)

	// 全ロール及び未認証でも使えるhandler
	e.GET("/api/me", meHandler)

	// ベンチマーカー向けAPI
	e.POST("/initialize", initializeHandler)
	// 他ノードからの初期化依頼（nginx は外に出さない）
	e.POST("/internal/initialize", internalInitializeHandler)

	e.HTTPErrorHandler = errorResponseHandler

	baseHost = getEnv("ISUCON_BASE_HOSTNAME", ".t.isucon.local")
	adminHost = getEnv("ISUCON_ADMIN_HOSTNAME", "admin.t.isucon.local")
	nodeSuffix = getEnv("ISUCON_NODE_ID", "1")
	idCounter = time.Now().UnixMicro()

	keyFilename := getEnv("ISUCON_JWT_KEY_FILE", "../public.pem")
	keysrc, err := os.ReadFile(keyFilename)
	if err != nil {
		e.Logger.Fatalf("error os.ReadFile: keyFilename=%s: %v", keyFilename, err)
	}
	jwtKey, _, err = jwk.DecodePEM(keysrc)
	if err != nil {
		e.Logger.Fatalf("error jwk.DecodePEM: %v", err)
	}

	adminDB, err = connectAdminDB()
	if err != nil {
		e.Logger.Fatalf("failed to connect db: %v", err)
		return
	}
	adminDB.SetMaxOpenConns(32)
	adminDB.SetMaxIdleConns(32)
	defer adminDB.Close()
	// 再起動直後は別ノードの MySQL がまだ上がっていないことがある
	for i := 0; i < 120; i++ {
		if err = adminDB.Ping(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	go func() { _ = http.ListenAndServe("127.0.0.1:6060", nil) }()

	// TLS を直接終端する入口（isu1 の nginx stream から SNI ハッシュで振られてくる）
	if tlsAddr := getEnv("ISUCON_TLS_ADDR", ""); tlsAddr != "" {
		go func() {
			srv := &http.Server{Addr: tlsAddr, Handler: frontHandler(e)}
			e.Logger.Fatal(srv.ListenAndServeTLS(
				getEnv("ISUCON_TLS_CERT", "/etc/nginx/tls/fullchain.pem"),
				getEnv("ISUCON_TLS_KEY", "/etc/nginx/tls/key.pem"),
			))
		}()
	}

	port := getEnv("SERVER_APP_PORT", "3000")
	serverPort := fmt.Sprintf(":%s", port)
	e.Logger.Fatal(e.Start(serverPort))
}

// エラー処理関数
func errorResponseHandler(err error, c echo.Context) {
	var he *echo.HTTPError
	if errors.As(err, &he) {
		c.JSON(he.Code, FailureResult{
			Status: false,
		})
		return
	}
	c.Logger().Errorf("error at %s: %s", c.Path(), err.Error())
	c.JSON(http.StatusInternalServerError, FailureResult{
		Status: false,
	})
}

type SuccessResult struct {
	Status bool `json:"status"`
	Data   any  `json:"data,omitempty"`
}

type FailureResult struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
}

// アクセスしてきた人の情報
type Viewer struct {
	role       string
	playerID   string
	tenantName string
	tenantID   int64
}

type tokenClaims struct {
	sub  string
	role string
	aud  string
	exp  time.Time
}

// JWT を検証して claims を返す。検証済みトークンはキャッシュする（RSA 検証を毎回やらない）
func verifyToken(tokenStr string) (*tokenClaims, error) {
	if v, ok := tokenCache.Load(tokenStr); ok {
		tc := v.(*tokenClaims)
		if tc.exp.IsZero() || time.Now().Before(tc.exp) {
			return tc, nil
		}
		tokenCache.Delete(tokenStr)
	}
	token, err := jwt.Parse(
		[]byte(tokenStr),
		jwt.WithKey(jwa.RS256, jwtKey),
	)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, fmt.Errorf("error jwt.Parse: %s", err.Error()))
	}
	if token.Subject() == "" {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, "invalid token: subject is not found in token")
	}
	tr, ok := token.Get("role")
	if !ok {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, "invalid token: role is not found")
	}
	var role string
	switch tr {
	case RoleAdmin, RoleOrganizer, RolePlayer:
		role = tr.(string)
	default:
		return nil, echo.NewHTTPError(http.StatusUnauthorized, "invalid token: invalid role")
	}
	// aud は1要素でテナント名がはいっている
	aud := token.Audience()
	if len(aud) != 1 {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, "invalid token: aud field is few or too much")
	}
	tc := &tokenClaims{sub: token.Subject(), role: role, aud: aud[0], exp: token.Expiration()}
	tokenCache.Store(tokenStr, tc)
	return tc, nil
}

// リクエストヘッダをパースしてViewerを返す
func parseViewer(c echo.Context) (*Viewer, error) {
	cookie, err := c.Request().Cookie(cookieName)
	if err != nil {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("cookie %s is not found", cookieName),
		)
	}
	tc, err := verifyToken(cookie.Value)
	if err != nil {
		return nil, err
	}
	tenant, err := retrieveTenantRowFromHeader(c)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, echo.NewHTTPError(http.StatusUnauthorized, "tenant not found")
		}
		return nil, fmt.Errorf("error retrieveTenantRowFromHeader at parseViewer: %w", err)
	}
	if tenant.Name == "admin" && tc.role != RoleAdmin {
		return nil, echo.NewHTTPError(http.StatusUnauthorized, "tenant not found")
	}

	if tenant.Name != tc.aud {
		return nil, echo.NewHTTPError(
			http.StatusUnauthorized,
			fmt.Sprintf("invalid token: tenant name is not match with %s", c.Request().Host),
		)
	}

	v := &Viewer{
		role:       tc.role,
		playerID:   tc.sub,
		tenantName: tenant.Name,
		tenantID:   tenant.ID,
	}
	return v, nil
}

var adminTenantRow = &TenantRow{Name: "admin", DisplayName: "admin"}

func retrieveTenantRowFromHeader(c echo.Context) (*TenantRow, error) {
	// JWTに入っているテナント名とHostヘッダのテナント名が一致しているか確認
	tenantName := strings.TrimSuffix(c.Request().Host, baseHost)

	// SaaS管理者用ドメイン
	if tenantName == "admin" {
		return adminTenantRow, nil
	}
	if v, ok := tenantByName.Load(tenantName); ok {
		return v.(*TenantRow), nil
	}

	// テナントの存在確認
	var tenant TenantRow
	if err := adminDB.GetContext(
		context.Background(),
		&tenant,
		"SELECT * FROM tenant WHERE name = ?",
		tenantName,
	); err != nil {
		return nil, fmt.Errorf("failed to Select tenant: name=%s, %w", tenantName, err)
	}
	tenantByName.Store(tenantName, &tenant)
	return &tenant, nil
}

type TenantRow struct {
	ID          int64  `db:"id"`
	Name        string `db:"name"`
	DisplayName string `db:"display_name"`
	CreatedAt   int64  `db:"created_at"`
	UpdatedAt   int64  `db:"updated_at"`
}

// 参加者を認可する
// 参加者向けAPIで呼ばれる。t.mu を (R)Lock した状態で呼ぶ
func (t *tenantT) authorizePlayer(id string) error {
	player, ok := t.players[id]
	if !ok {
		return echo.NewHTTPError(http.StatusUnauthorized, "player not found")
	}
	if player.Disq {
		return echo.NewHTTPError(http.StatusForbidden, "player is disqualified")
	}
	return nil
}

type TenantsAddHandlerResult struct {
	Tenant TenantWithBilling `json:"tenant"`
}

// SasS管理者用API
// テナントを追加する
// POST /api/admin/tenants/add
func tenantsAddHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.tenantName != "admin" {
		// admin: SaaS管理者用の特別なテナント名
		return echo.NewHTTPError(
			http.StatusNotFound,
			fmt.Sprintf("%s has not this API", v.tenantName),
		)
	}
	if v.role != RoleAdmin {
		return echo.NewHTTPError(http.StatusForbidden, "admin role required")
	}

	displayName := c.FormValue("display_name")
	name := c.FormValue("name")
	if err := validateTenantName(name); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := context.Background()
	now := time.Now().Unix()
	insertRes, err := adminDB.ExecContext(
		ctx,
		"INSERT INTO tenant (name, display_name, created_at, updated_at) VALUES (?, ?, ?, ?)",
		name, displayName, now, now,
	)
	if err != nil {
		if merr, ok := err.(*mysql.MySQLError); ok && merr.Number == 1062 { // duplicate entry
			return echo.NewHTTPError(http.StatusBadRequest, "duplicate tenant")
		}
		return fmt.Errorf(
			"error Insert tenant: name=%s, displayName=%s, createdAt=%d, updatedAt=%d, %w",
			name, displayName, now, now, err,
		)
	}

	id, err := insertRes.LastInsertId()
	if err != nil {
		return fmt.Errorf("error get LastInsertId: %w", err)
	}
	// テナント DB は担当ノードが初回アクセス時に作る（getTenant）

	res := TenantsAddHandlerResult{
		Tenant: TenantWithBilling{
			ID:          strconv.FormatInt(id, 10),
			Name:        name,
			DisplayName: displayName,
			BillingYen:  0,
		},
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

// テナント名が規則に沿っているかチェックする
func validateTenantName(name string) error {
	if tenantNameRegexp.MatchString(name) {
		return nil
	}
	return fmt.Errorf("invalid tenant name: %s", name)
}

type BillingReport struct {
	CompetitionID     string `json:"competition_id"`
	CompetitionTitle  string `json:"competition_title"`
	PlayerCount       int64  `json:"player_count"`        // スコアを登録した参加者数
	VisitorCount      int64  `json:"visitor_count"`       // ランキングを閲覧だけした(スコアを登録していない)参加者数
	BillingPlayerYen  int64  `json:"billing_player_yen"`  // 請求金額 スコアを登録した参加者分
	BillingVisitorYen int64  `json:"billing_visitor_yen"` // 請求金額 ランキングを閲覧だけした(スコアを登録していない)参加者分
	BillingYen        int64  `json:"billing_yen"`         // 合計請求金額
}

type TenantWithBilling struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	BillingYen  int64  `json:"billing"`
}

type TenantsBillingHandlerResult struct {
	Tenants []TenantWithBilling `json:"tenants"`
}

// SaaS管理者用API
// テナントごとの課金レポートを最大10件、テナントのid降順で取得する
// GET /api/admin/tenants/billing
// URL引数beforeを指定した場合、指定した値よりもidが小さいテナントの課金レポートを取得する
func tenantsBillingHandler(c echo.Context) error {
	if host := c.Request().Host; host != adminHost {
		return echo.NewHTTPError(
			http.StatusNotFound,
			fmt.Sprintf("invalid hostname %s", host),
		)
	}

	ctx := context.Background()
	if v, err := parseViewer(c); err != nil {
		return err
	} else if v.role != RoleAdmin {
		return echo.NewHTTPError(http.StatusForbidden, "admin role required")
	}

	before := c.QueryParam("before")
	var beforeID int64
	if before != "" {
		var err error
		beforeID, err = strconv.ParseInt(before, 10, 64)
		if err != nil {
			return echo.NewHTTPError(
				http.StatusBadRequest,
				fmt.Sprintf("failed to parse query parameter 'before': %s", err.Error()),
			)
		}
	}
	// テナントごとの請求額は大会終了時に tenant_billing に積んである
	type row struct {
		ID          int64  `db:"id"`
		Name        string `db:"name"`
		DisplayName string `db:"display_name"`
		Billing     int64  `db:"billing"`
	}
	rows := []row{}
	q := "SELECT t.id AS id, t.name AS name, t.display_name AS display_name, IFNULL(b.billing, 0) AS billing FROM tenant t LEFT JOIN tenant_billing b ON b.tenant_id = t.id"
	var err error
	if beforeID != 0 {
		err = adminDB.SelectContext(ctx, &rows, q+" WHERE t.id < ? ORDER BY t.id DESC LIMIT 10", beforeID)
	} else {
		err = adminDB.SelectContext(ctx, &rows, q+" ORDER BY t.id DESC LIMIT 10")
	}
	if err != nil {
		return fmt.Errorf("error Select tenant: %w", err)
	}
	tenantBillings := make([]TenantWithBilling, 0, len(rows))
	for _, r := range rows {
		tenantBillings = append(tenantBillings, TenantWithBilling{
			ID:          strconv.FormatInt(r.ID, 10),
			Name:        r.Name,
			DisplayName: r.DisplayName,
			BillingYen:  r.Billing,
		})
	}
	return c.JSON(http.StatusOK, SuccessResult{
		Status: true,
		Data: TenantsBillingHandlerResult{
			Tenants: tenantBillings,
		},
	})
}

type PlayerDetail struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name"`
	IsDisqualified bool   `json:"is_disqualified"`
}

type PlayersListHandlerResult struct {
	Players []PlayerDetail `json:"players"`
}

// テナント管理者向けAPI
// GET /api/organizer/players
// 参加者一覧を返す
func playersListHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return err
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return fmt.Errorf("error getTenant: %w", err)
	}

	t.mu.Lock() // playersDesc がキャッシュを作るので書き込みロック
	var pds []PlayerDetail
	for _, p := range t.playersDesc() {
		pds = append(pds, PlayerDetail{
			ID:             p.ID,
			DisplayName:    p.DisplayName,
			IsDisqualified: p.Disq,
		})
	}
	t.mu.Unlock()

	res := PlayersListHandlerResult{
		Players: pds,
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

type PlayersAddHandlerResult struct {
	Players []PlayerDetail `json:"players"`
}

// テナント管理者向けAPI
// GET /api/organizer/players/add
// テナントに参加者を追加する
func playersAddHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	params, err := c.FormParams()
	if err != nil {
		return fmt.Errorf("error c.FormParams: %w", err)
	}
	displayNames := params["display_name[]"]

	pds := make([]PlayerDetail, 0, len(displayNames))
	t.mu.Lock()
	defer t.mu.Unlock()
	tx, err := t.db.Begin()
	if err != nil {
		return fmt.Errorf("error begin: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	added := make([]*playerT, 0, len(displayNames))
	for _, displayName := range displayNames {
		id := dispenseID()
		if _, err := tx.Exec(
			"INSERT INTO player (id, tenant_id, display_name, is_disqualified, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
			id, v.tenantID, displayName, false, now, now,
		); err != nil {
			return fmt.Errorf("error Insert player at tenantDB: id=%s, %w", id, err)
		}
		added = append(added, &playerT{ID: id, DisplayName: displayName, CreatedAt: now})
		pds = append(pds, PlayerDetail{
			ID:             id,
			DisplayName:    displayName,
			IsDisqualified: false,
		})
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("error commit: %w", err)
	}
	for _, p := range added {
		t.players[p.ID] = p
		t.playerList = append(t.playerList, p)
	}
	t.playerDesc = nil

	res := PlayersAddHandlerResult{
		Players: pds,
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

type PlayerDisqualifiedHandlerResult struct {
	Player PlayerDetail `json:"player"`
}

// テナント管理者向けAPI
// POST /api/organizer/player/:player_id/disqualified
// 参加者を失格にする
func playerDisqualifiedHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	playerID := c.Param("player_id")

	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.players[playerID]
	if !ok {
		// 存在しないプレイヤー
		return echo.NewHTTPError(http.StatusNotFound, "player not found")
	}
	now := time.Now().Unix()
	if _, err := t.db.Exec(
		"UPDATE player SET is_disqualified = ?, updated_at = ? WHERE id = ?",
		true, now, playerID,
	); err != nil {
		return fmt.Errorf("error Update player: id=%s, %w", playerID, err)
	}
	p.Disq = true

	res := PlayerDisqualifiedHandlerResult{
		Player: PlayerDetail{
			ID:             p.ID,
			DisplayName:    p.DisplayName,
			IsDisqualified: p.Disq,
		},
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

type CompetitionDetail struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	IsFinished bool   `json:"is_finished"`
}

type CompetitionsAddHandlerResult struct {
	Competition CompetitionDetail `json:"competition"`
}

// テナント管理者向けAPI
// POST /api/organizer/competitions/add
// 大会を追加する
func competitionsAddHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	title := c.FormValue("title")

	now := time.Now().Unix()
	id := dispenseID()
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, err := t.db.Exec(
		"INSERT INTO competition (id, tenant_id, title, finished_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, v.tenantID, title, sql.NullInt64{}, now, now,
	); err != nil {
		return fmt.Errorf("error Insert competition: id=%s, tenant_id=%d, %w", id, v.tenantID, err)
	}
	comp := &compT{ID: id, Title: title, CreatedAt: now, scores: map[string]scoreT{}, visitors: map[string]struct{}{}}
	t.comps[id] = comp
	t.compList = append(t.compList, comp)
	t.compDesc = nil

	res := CompetitionsAddHandlerResult{
		Competition: CompetitionDetail{
			ID:         id,
			Title:      title,
			IsFinished: false,
		},
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}

// テナント管理者向けAPI
// POST /api/organizer/competition/:competition_id/finish
// 大会を終了する
func competitionFinishHandler(c echo.Context) error {
	ctx := context.Background()
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	} else if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	id := c.Param("competition_id")
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "competition_id required")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	comp, ok := t.comps[id]
	if !ok {
		// 存在しない大会
		return echo.NewHTTPError(http.StatusNotFound, "competition not found")
	}
	if comp.Finished {
		return c.JSON(http.StatusOK, SuccessResult{Status: true})
	}

	// 終了と同時に請求額を確定する（終了後の閲覧は課金対象外で、スコアも変わらない）
	now := time.Now().Unix()
	playerCount := int64(len(comp.scores))
	var visitorCount int64
	for pid := range comp.visitors {
		if _, scored := comp.scores[pid]; !scored {
			visitorCount++
		}
	}
	tx, err := t.db.Begin()
	if err != nil {
		return fmt.Errorf("error begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE competition SET finished_at = ?, updated_at = ? WHERE id = ?", now, now, id); err != nil {
		return fmt.Errorf("error Update competition: id=%s, %w", id, err)
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO billing_report (competition_id, player_count, visitor_count) VALUES (?, ?, ?)", id, playerCount, visitorCount); err != nil {
		return fmt.Errorf("error Insert billing_report: id=%s, %w", id, err)
	}
	if _, err := tx.Exec("DELETE FROM visit_history WHERE competition_id = ?", id); err != nil {
		return fmt.Errorf("error Delete visit_history: id=%s, %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("error commit: %w", err)
	}
	if yen := 100*playerCount + 10*visitorCount; yen > 0 {
		if _, err := adminDB.ExecContext(ctx,
			"INSERT INTO tenant_billing (tenant_id, billing) VALUES (?, ?) ON DUPLICATE KEY UPDATE billing = billing + VALUES(billing)",
			v.tenantID, yen,
		); err != nil {
			return fmt.Errorf("error Upsert tenant_billing: tenant=%d, %w", v.tenantID, err)
		}
	}
	comp.Finished = true
	comp.FinishedAt = now
	comp.playerCount = playerCount
	comp.visitorCount = visitorCount
	comp.visitors = nil
	return c.JSON(http.StatusOK, SuccessResult{Status: true})
}

type ScoreHandlerResult struct {
	Rows int64 `json:"rows"`
}

// テナント管理者向けAPI
// POST /api/organizer/competition/:competition_id/score
// 大会のスコアをCSVでアップロードする
func competitionScoreHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	competitionID := c.Param("competition_id")
	if competitionID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "competition_id required")
	}
	t.mu.RLock()
	comp, ok := t.comps[competitionID]
	finished := ok && comp.Finished
	t.mu.RUnlock()
	if !ok {
		// 存在しない大会
		return echo.NewHTTPError(http.StatusNotFound, "competition not found")
	}
	if finished {
		res := FailureResult{
			Status:  false,
			Message: "competition is finished",
		}
		return c.JSON(http.StatusBadRequest, res)
	}

	fh, err := c.FormFile("scores")
	if err != nil {
		return fmt.Errorf("error c.FormFile(scores): %w", err)
	}
	f, err := fh.Open()
	if err != nil {
		return fmt.Errorf("error fh.Open FormFile(scores): %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	headers, err := r.Read()
	if err != nil {
		return fmt.Errorf("error r.Read at header: %w", err)
	}
	if !reflect.DeepEqual(headers, []string{"player_id", "score"}) {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid CSV headers")
	}
	r.ReuseRecord = true
	type csvRow struct {
		playerID string
		scoreStr string
	}
	csvRows := []csvRow{}
	for {
		row, err := r.Read()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error r.Read at rows: %w", err)
		}
		if len(row) != 2 {
			return fmt.Errorf("row must have two columns: %#v", row)
		}
		csvRows = append(csvRows, csvRow{row[0], row[1]})
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if comp.Finished {
		return c.JSON(http.StatusBadRequest, FailureResult{Status: false, Message: "competition is finished"})
	}
	// 同じ参加者が複数回出たら最後の行（row_num 最大）だけ採用する
	scores := make(map[string]scoreT, len(csvRows))
	for i, row := range csvRows {
		if _, ok := t.players[row.playerID]; !ok {
			// 存在しない参加者が含まれている
			return echo.NewHTTPError(
				http.StatusBadRequest,
				fmt.Sprintf("player not found: %s", row.playerID),
			)
		}
		score, err := strconv.ParseInt(row.scoreStr, 10, 64)
		if err != nil {
			return echo.NewHTTPError(
				http.StatusBadRequest,
				fmt.Sprintf("error strconv.ParseUint: scoreStr=%s, %s", row.scoreStr, err),
			)
		}
		scores[row.playerID] = scoreT{Score: score, RowNum: int64(i + 1)}
	}

	tx, err := t.db.Begin()
	if err != nil {
		return fmt.Errorf("error begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM player_score WHERE competition_id = ?", competitionID); err != nil {
		return fmt.Errorf("error Delete player_score: competitionID=%s, %w", competitionID, err)
	}
	ins, err := tx.Prepare("INSERT INTO player_score (competition_id, player_id, score, row_num) VALUES (?, ?, ?, ?)")
	if err != nil {
		return fmt.Errorf("error prepare: %w", err)
	}
	for pid, s := range scores {
		if _, err := ins.Exec(competitionID, pid, s.Score, s.RowNum); err != nil {
			ins.Close()
			return fmt.Errorf("error Insert player_score: playerID=%s, competitionID=%s, %w", pid, competitionID, err)
		}
	}
	ins.Close()
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("error commit: %w", err)
	}
	comp.scores = scores
	t.rebuildRanks(comp)

	return c.JSON(http.StatusOK, SuccessResult{
		Status: true,
		Data:   ScoreHandlerResult{Rows: int64(len(csvRows))},
	})
}

type BillingHandlerResult struct {
	Reports []BillingReport `json:"reports"`
}

// テナント管理者向けAPI
// GET /api/organizer/billing
// テナント内の課金レポートを取得する
func billingHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	t.mu.Lock() // compsDesc がキャッシュを作るので書き込みロック
	cs := t.compsDesc()
	tbrs := make([]BillingReport, 0, len(cs))
	for _, comp := range cs {
		tbrs = append(tbrs, comp.report())
	}
	t.mu.Unlock()

	res := SuccessResult{
		Status: true,
		Data: BillingHandlerResult{
			Reports: tbrs,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type PlayerScoreDetail struct {
	CompetitionTitle string `json:"competition_title"`
	Score            int64  `json:"score"`
}

type PlayerHandlerResult struct {
	Player PlayerDetail        `json:"player"`
	Scores []PlayerScoreDetail `json:"scores"`
}

// 参加者向けAPI
// GET /api/player/player/:player_id
// 参加者の詳細情報を取得する
func playerHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	t.mu.RLock()
	if err := t.authorizePlayer(v.playerID); err != nil {
		t.mu.RUnlock()
		return err
	}

	playerID := c.Param("player_id")
	if playerID == "" {
		t.mu.RUnlock()
		return echo.NewHTTPError(http.StatusBadRequest, "player_id is required")
	}
	p, ok := t.players[playerID]
	if !ok {
		t.mu.RUnlock()
		return echo.NewHTTPError(http.StatusNotFound, "player not found")
	}
	psds := make([]PlayerScoreDetail, 0, len(t.compList))
	for _, comp := range t.compList {
		if s, ok := comp.scores[playerID]; ok {
			psds = append(psds, PlayerScoreDetail{
				CompetitionTitle: comp.Title,
				Score:            s.Score,
			})
		}
	}
	pd := PlayerDetail{
		ID:             p.ID,
		DisplayName:    p.DisplayName,
		IsDisqualified: p.Disq,
	}
	t.mu.RUnlock()

	res := SuccessResult{
		Status: true,
		Data: PlayerHandlerResult{
			Player: pd,
			Scores: psds,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type CompetitionRank struct {
	Rank              int64  `json:"rank"`
	Score             int64  `json:"score"`
	PlayerID          string `json:"player_id"`
	PlayerDisplayName string `json:"player_display_name"`
	RowNum            int64  `json:"-"` // APIレスポンスのJSONには含まれない
}

type CompetitionRankingHandlerResult struct {
	Competition CompetitionDetail `json:"competition"`
	Ranks       []CompetitionRank `json:"ranks"`
}

// 参加者向けAPI
// GET /api/player/competition/:competition_id/ranking
// 大会ごとのランキングを取得する
func competitionRankingHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	t.mu.RLock()
	if err := t.authorizePlayer(v.playerID); err != nil {
		t.mu.RUnlock()
		return err
	}

	competitionID := c.Param("competition_id")
	if competitionID == "" {
		t.mu.RUnlock()
		return echo.NewHTTPError(http.StatusBadRequest, "competition_id is required")
	}

	// 大会の存在確認
	comp, ok := t.comps[competitionID]
	if !ok {
		t.mu.RUnlock()
		return echo.NewHTTPError(http.StatusNotFound, "competition not found")
	}

	// 開催中の大会だけ閲覧履歴を残す（終了後の閲覧は課金対象にならない）。同じ参加者は最初の1回だけ
	if !comp.Finished {
		comp.visitMu.Lock()
		_, seen := comp.visitors[v.playerID]
		if !seen {
			comp.visitors[v.playerID] = struct{}{}
		}
		comp.visitMu.Unlock()
		if !seen {
			if _, err := t.db.Exec(
				"INSERT INTO visit_history (competition_id, player_id, created_at) VALUES (?, ?, ?)",
				competitionID, v.playerID, time.Now().Unix(),
			); err != nil {
				t.mu.RUnlock()
				return fmt.Errorf("error Insert visit_history: playerID=%s, competitionID=%s, %w", v.playerID, competitionID, err)
			}
		}
	}

	var rankAfter int64
	rankAfterStr := c.QueryParam("rank_after")
	if rankAfterStr != "" {
		if rankAfter, err = strconv.ParseInt(rankAfterStr, 10, 64); err != nil {
			t.mu.RUnlock()
			return fmt.Errorf("error strconv.ParseUint: rankAfterStr=%s, %w", rankAfterStr, err)
		}
	}

	ranks := comp.ranks // 入稿のたびに新しいスライスに差し替わるので、ロックを外した後も読める
	cd := CompetitionDetail{
		ID:         comp.ID,
		Title:      comp.Title,
		IsFinished: comp.Finished,
	}
	t.mu.RUnlock()

	if rankAfter < 0 {
		rankAfter = 0
	}
	pagedRanks := []CompetitionRank{}
	if rankAfter < int64(len(ranks)) {
		end := rankAfter + 100
		if end > int64(len(ranks)) {
			end = int64(len(ranks))
		}
		pagedRanks = ranks[rankAfter:end]
	}

	res := SuccessResult{
		Status: true,
		Data: CompetitionRankingHandlerResult{
			Competition: cd,
			Ranks:       pagedRanks,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type CompetitionsHandlerResult struct {
	Competitions []CompetitionDetail `json:"competitions"`
}

// 参加者向けAPI
// GET /api/player/competitions
// 大会の一覧を取得する
func playerCompetitionsHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RolePlayer {
		return echo.NewHTTPError(http.StatusForbidden, "role player required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	t.mu.RLock()
	err = t.authorizePlayer(v.playerID)
	t.mu.RUnlock()
	if err != nil {
		return err
	}
	return competitionsHandler(c, t)
}

// テナント管理者向けAPI
// GET /api/organizer/competitions
// 大会の一覧を取得する
func organizerCompetitionsHandler(c echo.Context) error {
	v, err := parseViewer(c)
	if err != nil {
		return err
	}
	if v.role != RoleOrganizer {
		return echo.NewHTTPError(http.StatusForbidden, "role organizer required")
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return err
	}

	return competitionsHandler(c, t)
}

func competitionsHandler(c echo.Context, t *tenantT) error {
	t.mu.Lock() // compsDesc がキャッシュを作るので書き込みロック
	cs := t.compsDesc()
	cds := make([]CompetitionDetail, 0, len(cs))
	for _, comp := range cs {
		cds = append(cds, CompetitionDetail{
			ID:         comp.ID,
			Title:      comp.Title,
			IsFinished: comp.Finished,
		})
	}
	t.mu.Unlock()

	res := SuccessResult{
		Status: true,
		Data: CompetitionsHandlerResult{
			Competitions: cds,
		},
	}
	return c.JSON(http.StatusOK, res)
}

type TenantDetail struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

type MeHandlerResult struct {
	Tenant   *TenantDetail `json:"tenant"`
	Me       *PlayerDetail `json:"me"`
	Role     string        `json:"role"`
	LoggedIn bool          `json:"logged_in"`
}

// 共通API
// GET /api/me
// JWTで認証した結果、テナントやユーザ情報を返す
func meHandler(c echo.Context) error {
	tenant, err := retrieveTenantRowFromHeader(c)
	if err != nil {
		return fmt.Errorf("error retrieveTenantRowFromHeader: %w", err)
	}
	td := &TenantDetail{
		Name:        tenant.Name,
		DisplayName: tenant.DisplayName,
	}
	v, err := parseViewer(c)
	if err != nil {
		var he *echo.HTTPError
		if ok := errors.As(err, &he); ok && he.Code == http.StatusUnauthorized {
			return c.JSON(http.StatusOK, SuccessResult{
				Status: true,
				Data: MeHandlerResult{
					Tenant:   td,
					Me:       nil,
					Role:     RoleNone,
					LoggedIn: false,
				},
			})
		}
		return fmt.Errorf("error parseViewer: %w", err)
	}
	if v.role == RoleAdmin || v.role == RoleOrganizer {
		return c.JSON(http.StatusOK, SuccessResult{
			Status: true,
			Data: MeHandlerResult{
				Tenant:   td,
				Me:       nil,
				Role:     v.role,
				LoggedIn: true,
			},
		})
	}

	t, err := getTenant(v.tenantID)
	if err != nil {
		return fmt.Errorf("error getTenant: %w", err)
	}
	t.mu.RLock()
	p, ok := t.players[v.playerID]
	var me *PlayerDetail
	if ok {
		me = &PlayerDetail{
			ID:             p.ID,
			DisplayName:    p.DisplayName,
			IsDisqualified: p.Disq,
		}
	}
	t.mu.RUnlock()
	if !ok {
		return c.JSON(http.StatusOK, SuccessResult{
			Status: true,
			Data: MeHandlerResult{
				Tenant:   td,
				Me:       nil,
				Role:     RoleNone,
				LoggedIn: false,
			},
		})
	}

	return c.JSON(http.StatusOK, SuccessResult{
		Status: true,
		Data: MeHandlerResult{
			Tenant:   td,
			Me:       me,
			Role:     v.role,
			LoggedIn: true,
		},
	})
}

type InitializeHandlerResult struct {
	Lang string `json:"lang"`
}

// このノードのテナントデータとキャッシュを初期状態に戻す
func initializeLocal() error {
	resetTenants()
	tenantByName.Range(func(k, _ any) bool { tenantByName.Delete(k); return true })
	tokenCache.Range(func(k, _ any) bool { tokenCache.Delete(k); return true })
	if err := restoreTenantDBs(); err != nil {
		return err
	}
	// 初期テナントを先に読み込んでおく（負荷走行中の初回アクセスで待たせない）
	var wg sync.WaitGroup
	ids := make(chan int64, 100)
	for i := int64(1); i <= 100; i++ {
		ids <- i
	}
	close(ids)
	errs := make(chan error, 4)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for id := range ids {
				if _, err := os.Stat(tenantDBPath(id)); err != nil {
					continue
				}
				if _, err := getTenant(id); err != nil {
					select {
					case errs <- err:
					default:
					}
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errs:
		return err
	default:
	}
	return nil
}

func internalInitializeHandler(c echo.Context) error {
	if err := initializeLocal(); err != nil {
		return fmt.Errorf("error initializeLocal: %w", err)
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true})
}

// ベンチマーカー向けAPI
// POST /initialize
// ベンチマーカーが起動したときに最初に呼ぶ
// データベースの初期化などが実行されるため、スキーマを変更した場合などは適宜改変すること
func initializeHandler(c echo.Context) error {
	// 他ノードの初期化を並行して依頼する
	peers := strings.Fields(strings.ReplaceAll(getEnv("ISUCON_PEERS", ""), ",", " "))
	errCh := make(chan error, len(peers)+1)
	for _, peer := range peers {
		go func(peer string) {
			resp, err := http.Post("http://"+peer+"/internal/initialize", "text/plain", nil)
			if err != nil {
				errCh <- fmt.Errorf("peer %s: %w", peer, err)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("peer %s: status %d", peer, resp.StatusCode)
				return
			}
			errCh <- nil
		}(peer)
	}
	go func() {
		for _, q := range []string{
			"DELETE FROM tenant WHERE id > 100",
			"DELETE FROM tenant_billing",
			"INSERT INTO tenant_billing (tenant_id, billing) SELECT tenant_id, billing FROM tenant_billing_init",
		} {
			if _, err := adminDB.Exec(q); err != nil {
				errCh <- fmt.Errorf("error init admin db: %s: %w", q, err)
				return
			}
		}
		errCh <- nil
	}()
	localErr := initializeLocal()
	for i := 0; i < len(peers)+1; i++ {
		if err := <-errCh; err != nil {
			return err
		}
	}
	if localErr != nil {
		return fmt.Errorf("error initializeLocal: %w", localErr)
	}
	res := InitializeHandlerResult{
		Lang: "go",
	}
	return c.JSON(http.StatusOK, SuccessResult{Status: true, Data: res})
}
