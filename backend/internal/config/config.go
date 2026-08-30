// Package config は環境変数から設定を読み、起動時に検証する。
//
// 方針:
//   - **欠けている設定は起動時にまとめて落とす**。API キーの欠落を実行時に発見すると、
//     ユーザーのリクエストを 1 件無駄にした上でしか気づけない。
//   - 問題は 1 つ見つけるたびに返さず、**全部集めてから 1 度に報告する**。
//     デプロイのたびに env を 1 個ずつ直す往復を避けるため。
//   - 秘密情報は Secret 型で保持する。ログに %v で出しても値が漏れない。
//   - 空文字は「未設定」として扱う。
//
// 環境変数の一覧は README ではなくこのファイルが正とする。
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Secret はログに出しても漏れない秘密文字列。
type Secret string

// String は fmt の %v / %s 経由の出力を伏せ字にする。
func (s Secret) String() string {
	if s == "" {
		return "(未設定)"
	}
	return "****"
}

// Value は実際の値を取り出す。外部 API を呼ぶ直前でのみ使う。
func (s Secret) Value() string { return string(s) }

func (s Secret) Empty() bool { return s == "" }

// Env は動作環境。
type Env string

const (
	EnvLocal      Env = "local"
	EnvStaging    Env = "staging"
	EnvProduction Env = "production"
)

func (e Env) IsProduction() bool { return e == EnvProduction }

// Config は起動に必要な設定の全体。
type Config struct {
	App      App
	Server   Server
	Database Database
	Cache    Cache
	Rakuten  Rakuten
	Google   Google
	LLM      LLM
	Plan     Plan
}

type App struct {
	Env Env // APP_ENV
	// BaseURL は共有 URL（shareUrl）と OGP の組み立てに使う。
	BaseURL  string // APP_BASE_URL
	LogLevel string // LOG_LEVEL
	Version  string // APP_VERSION（コミット SHA。/health が返す）
}

type Server struct {
	Port int // PORT

	ReadTimeout     time.Duration // SERVER_READ_TIMEOUT
	WriteTimeout    time.Duration // SERVER_WRITE_TIMEOUT
	IdleTimeout     time.Duration // SERVER_IDLE_TIMEOUT
	ShutdownTimeout time.Duration // SERVER_SHUTDOWN_TIMEOUT

	// CORSOrigins は Next.js のオリジン。ワイルドカードは本番で禁止。
	CORSOrigins []string // CORS_ORIGINS（カンマ区切り）
	// RateLimitPerMinute は 1 IP あたりの上限。LLM 課金を守る最後の砦。
	RateLimitPerMinute int // RATE_LIMIT_PER_MINUTE
}

type Database struct {
	URL             Secret        // DATABASE_URL
	MaxOpenConns    int           // DB_MAX_OPEN_CONNS
	MaxIdleConns    int           // DB_MAX_IDLE_CONNS
	ConnMaxLifetime time.Duration // DB_CONN_MAX_LIFETIME
}

// Cache は Places / Routes のコスト削減に必須。無効化は開発時のみを想定する。
type Cache struct {
	Enabled   bool          // CACHE_ENABLED
	URL       Secret        // REDIS_URL
	PlacesTTL time.Duration // CACHE_PLACES_TTL
	RoutesTTL time.Duration // CACHE_ROUTES_TTL
}

type Rakuten struct {
	ApplicationID Secret // RAKUTEN_APPLICATION_ID
	// AffiliateID が欠けるとアフィリエイト収益がゼロになる。本番では必須。
	AffiliateID string        // RAKUTEN_AFFILIATE_ID
	Timeout     time.Duration // RAKUTEN_TIMEOUT
	// QPS は楽天ウェブサービスのレート制限（概ね 1req/sec）。
	// 並行 fan-out といっても楽天だけは直列化が要る。
	QPS float64 // RAKUTEN_QPS
}

type Google struct {
	MapsAPIKey    Secret        // GOOGLE_MAPS_API_KEY（Places / Routes 共通）
	PlacesTimeout time.Duration // GOOGLE_PLACES_TIMEOUT
	RoutesTimeout time.Duration // GOOGLE_ROUTES_TIMEOUT
	// PhotoProxyBaseURL は Places の写真を配る自前 CDN。API キー秘匿とキャッシュのため直リンクしない。
	PhotoProxyBaseURL string // PHOTO_PROXY_BASE_URL
}

type LLM struct {
	// Enabled が false なら composer を丸ごと飛ばし、ルールベースで時系列だけ組む。
	// LLM が炎上したときに機能を落とさず逃げる非常口。
	Enabled   bool          // LLM_ENABLED
	Timeout   time.Duration // LLM_TIMEOUT
	Anthropic AnthropicLLM
	Gemini    GeminiLLM
	// MaxRepairAttempts は Validator 不合格による再生成の上限。
	// 1 を超えると遅延と課金が跳ねるので既定は 1 回。
	MaxRepairAttempts int    // LLM_MAX_REPAIR_ATTEMPTS
	SchemaVersion     string // LLM_SCHEMA_VERSION（meta.llm に載る）
	PromptVersion     string // LLM_PROMPT_VERSION
}

type AnthropicLLM struct {
	APIKey    Secret // ANTHROPIC_API_KEY
	Model     string // ANTHROPIC_MODEL
	MaxTokens int    // ANTHROPIC_MAX_TOKENS
	// Effort は構造化出力の思考量。候補の選択と作文が主なので medium で足りる。
	Effort string // ANTHROPIC_EFFORT
}

// Gemini は Anthropic が落ちたときのフォールバック先。
type GeminiLLM struct {
	APIKey Secret // GEMINI_API_KEY
	Model  string // GEMINI_MODEL
}

func (g GeminiLLM) Configured() bool { return !g.APIKey.Empty() }

type Plan struct {
	// TTL は空室情報の鮮度保証。既定はドメインの model.PlanTTL に合わせる。
	TTL time.Duration // PLAN_TTL
	// CollectTimeout は外部 3 API の収集全体に許す時間。
	// これを超えたぶんは部分失敗として切り捨て、partial で返す。
	CollectTimeout time.Duration // PLAN_COLLECT_TIMEOUT
	// MaxCandidatesPerCategory はリクエストの options 値に対する上限（コストの天井）。
	MaxCandidatesPerCategory int // PLAN_MAX_CANDIDATES
}

// Error は設定の不備をまとめて報告する。main はこれを出して即座に終了する。
type Error struct {
	Problems []string
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "設定が不正です（%d 件）:", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// Load は環境変数を読み、検証済みの設定を返す。
// 不備があれば *Error にすべての問題を詰めて返すので、main は Fatal で落とすだけでよい。
func Load() (*Config, error) {
	l := &loader{}

	env := Env(l.str("APP_ENV", string(EnvLocal)))
	prod := env.IsProduction()

	cfg := &Config{
		App: App{
			Env:      env,
			BaseURL:  l.url("APP_BASE_URL", "http://localhost:3000", prod),
			LogLevel: l.str("LOG_LEVEL", "info"),
			Version:  l.str("APP_VERSION", "dev"),
		},
		Server: Server{
			Port:               l.intv("PORT", 8080),
			ReadTimeout:        l.dur("SERVER_READ_TIMEOUT", 10*time.Second),
			WriteTimeout:       l.dur("SERVER_WRITE_TIMEOUT", 30*time.Second),
			IdleTimeout:        l.dur("SERVER_IDLE_TIMEOUT", 120*time.Second),
			ShutdownTimeout:    l.dur("SERVER_SHUTDOWN_TIMEOUT", 15*time.Second),
			CORSOrigins:        l.csv("CORS_ORIGINS", []string{"http://localhost:3000"}),
			RateLimitPerMinute: l.intv("RATE_LIMIT_PER_MINUTE", 30),
		},
		Database: Database{
			URL:             l.secret("DATABASE_URL", true),
			MaxOpenConns:    l.intv("DB_MAX_OPEN_CONNS", 25),
			MaxIdleConns:    l.intv("DB_MAX_IDLE_CONNS", 5),
			ConnMaxLifetime: l.dur("DB_CONN_MAX_LIFETIME", 30*time.Minute),
		},
		Rakuten: Rakuten{
			ApplicationID: l.secret("RAKUTEN_APPLICATION_ID", true),
			AffiliateID:   l.str("RAKUTEN_AFFILIATE_ID", ""),
			Timeout:       l.dur("RAKUTEN_TIMEOUT", 5*time.Second),
			QPS:           l.float("RAKUTEN_QPS", 1.0),
		},
		Google: Google{
			MapsAPIKey:        l.secret("GOOGLE_MAPS_API_KEY", true),
			PlacesTimeout:     l.dur("GOOGLE_PLACES_TIMEOUT", 5*time.Second),
			RoutesTimeout:     l.dur("GOOGLE_ROUTES_TIMEOUT", 5*time.Second),
			PhotoProxyBaseURL: l.url("PHOTO_PROXY_BASE_URL", "", false),
		},
		Plan: Plan{
			TTL:                      l.dur("PLAN_TTL", model.PlanTTL),
			CollectTimeout:           l.dur("PLAN_COLLECT_TIMEOUT", 12*time.Second),
			MaxCandidatesPerCategory: l.intv("PLAN_MAX_CANDIDATES", model.DefaultMaxCandidatesPerCategory),
		},
	}

	cacheEnabled := l.boolv("CACHE_ENABLED", true)
	cfg.Cache = Cache{
		Enabled: cacheEnabled,
		// キャッシュを使うなら接続先は必須。Places / Routes の課金に直結する。
		URL:       l.secret("REDIS_URL", cacheEnabled),
		PlacesTTL: l.dur("CACHE_PLACES_TTL", 6*time.Hour),
		RoutesTTL: l.dur("CACHE_ROUTES_TTL", time.Hour),
	}

	llmEnabled := l.boolv("LLM_ENABLED", true)
	cfg.LLM = LLM{
		Enabled: llmEnabled,
		Timeout: l.dur("LLM_TIMEOUT", 30*time.Second),
		Anthropic: AnthropicLLM{
			APIKey:    l.secret("ANTHROPIC_API_KEY", false),
			Model:     l.str("ANTHROPIC_MODEL", "claude-opus-5"),
			MaxTokens: l.intv("ANTHROPIC_MAX_TOKENS", 8000),
			Effort:    l.str("ANTHROPIC_EFFORT", "medium"),
		},
		Gemini: GeminiLLM{
			APIKey: l.secret("GEMINI_API_KEY", false),
			// 既定値を置かないのは、設計書が型番を指定していない提供元だから。
			// 使うなら型番を明示させる（勝手な既定値で存在しないモデルを叩かせない）。
			Model: l.str("GEMINI_MODEL", ""),
		},
		MaxRepairAttempts: l.intv("LLM_MAX_REPAIR_ATTEMPTS", 1),
		SchemaVersion:     l.str("LLM_SCHEMA_VERSION", "plan_output.v1"),
		PromptVersion:     l.str("LLM_PROMPT_VERSION", "plan_v1"),
	}

	problems := append(l.problems, cfg.validate()...)
	if len(problems) > 0 {
		return nil, &Error{Problems: problems}
	}
	return cfg, nil
}

// validate は値そのものの妥当性と、項目どうしの整合を見る。
func (c *Config) validate() []string {
	var p []string

	switch c.App.Env {
	case EnvLocal, EnvStaging, EnvProduction:
	default:
		p = append(p, fmt.Sprintf("APP_ENV: local / staging / production のいずれかを指定してください（現在: %q）", c.App.Env))
	}

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		p = append(p, fmt.Sprintf("PORT: 1〜65535 で指定してください（現在: %d）", c.Server.Port))
	}
	if c.Server.RateLimitPerMinute < 1 {
		p = append(p, "RATE_LIMIT_PER_MINUTE: 1 以上で指定してください")
	}

	if c.Plan.MaxCandidatesPerCategory < 3 || c.Plan.MaxCandidatesPerCategory > 20 {
		// openapi.yaml の options.maxCandidatesPerCategory と同じ範囲に揃える。
		p = append(p, fmt.Sprintf("PLAN_MAX_CANDIDATES: 3〜20 で指定してください（現在: %d）", c.Plan.MaxCandidatesPerCategory))
	}
	if c.Plan.TTL <= 0 {
		p = append(p, "PLAN_TTL: 正の値を指定してください")
	}

	if c.Rakuten.QPS <= 0 {
		p = append(p, "RAKUTEN_QPS: 正の値を指定してください（楽天は概ね 1req/sec）")
	}

	// LLM を使うなら、少なくとも 1 つの提供元が要る。
	if c.LLM.Enabled {
		if c.LLM.Anthropic.APIKey.Empty() && !c.LLM.Gemini.Configured() {
			p = append(p, "ANTHROPIC_API_KEY / GEMINI_API_KEY: LLM_ENABLED=true なら少なくとも一方が必要です（文章生成を止めるなら LLM_ENABLED=false）")
		}
		if c.LLM.Gemini.Configured() && c.LLM.Gemini.Model == "" {
			p = append(p, "GEMINI_MODEL: GEMINI_API_KEY を設定するならモデル名も必要です")
		}
		if c.LLM.Anthropic.MaxTokens < 1000 {
			p = append(p, "ANTHROPIC_MAX_TOKENS: 1000 以上で指定してください（構造化出力が途中で切れます）")
		}
		if c.LLM.MaxRepairAttempts < 0 || c.LLM.MaxRepairAttempts > 3 {
			p = append(p, "LLM_MAX_REPAIR_ATTEMPTS: 0〜3 で指定してください（大きいほど遅延と課金が跳ねます）")
		}
	}

	// ── 本番でだけ厳しくする項目 ──
	if c.App.Env.IsProduction() {
		if c.Rakuten.AffiliateID == "" {
			// 気づかないまま収益がゼロになるのが最悪なので、起動を止める。
			p = append(p, "RAKUTEN_AFFILIATE_ID: 本番では必須です（未設定だとアフィリエイト収益がゼロになります）")
		}
		if c.Google.PhotoProxyBaseURL == "" {
			p = append(p, "PHOTO_PROXY_BASE_URL: 本番では必須です（Places の写真を直リンクすると API キーが露出します）")
		}
		for _, o := range c.Server.CORSOrigins {
			if o == "*" {
				p = append(p, "CORS_ORIGINS: 本番でワイルドカードは使えません")
			}
		}
		if !strings.HasPrefix(c.App.BaseURL, "https://") {
			p = append(p, "APP_BASE_URL: 本番では https:// で始まる必要があります")
		}
	}

	return p
}

// ── 環境変数の読み取り ────────────────────────────────

type loader struct {
	problems []string
}

func (l *loader) fail(format string, args ...any) {
	l.problems = append(l.problems, fmt.Sprintf(format, args...))
}

// lookup は空文字を「未設定」として扱う。
func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return "", false
	}
	return strings.TrimSpace(v), true
}

func (l *loader) str(key, def string) string {
	if v, ok := lookup(key); ok {
		return v
	}
	return def
}

func (l *loader) secret(key string, required bool) Secret {
	v, ok := lookup(key)
	if !ok && required {
		l.fail("%s: 必須です", key)
	}
	return Secret(v)
}

func (l *loader) intv(key string, def int) int {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		l.fail("%s: 整数として解釈できません（%q）", key, v)
		return def
	}
	return n
}

func (l *loader) float(key string, def float64) float64 {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		l.fail("%s: 数値として解釈できません（%q）", key, v)
		return def
	}
	return f
}

func (l *loader) boolv(key string, def bool) bool {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.fail("%s: true / false で指定してください（%q）", key, v)
		return def
	}
	return b
}

// dur は "5s" / "30m" のような Go の期間表記を受け取る。
func (l *loader) dur(key string, def time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		l.fail("%s: 期間として解釈できません（%q、例: 30s / 15m）", key, v)
		return def
	}
	if d <= 0 {
		l.fail("%s: 正の値を指定してください（%q）", key, v)
		return def
	}
	return d
}

func (l *loader) csv(key string, def []string) []string {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func (l *loader) url(key, def string, required bool) string {
	v, ok := lookup(key)
	if !ok {
		if required {
			l.fail("%s: 必須です", key)
		}
		return def
	}
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || u.Host == "" {
		l.fail("%s: スキーム付きの URL を指定してください（%q）", key, v)
		return def
	}
	return strings.TrimRight(v, "/")
}

// AsError は err を *Error に変換する。main でのメッセージ整形に使う。
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
