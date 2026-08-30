package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// 必須項目。テストごとに埋めて、そこから 1 つずつ壊す。
func setMinimum(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://localhost:5432/lifestyle")
	t.Setenv("REDIS_URL", "redis://localhost:6379")
	t.Setenv("GOOGLE_MAPS_API_KEY", "dummy-google-key")
	t.Setenv("RAKUTEN_APPLICATION_ID", "dummy-rakuten-id")
	t.Setenv("ANTHROPIC_API_KEY", "dummy-anthropic-key")
	// 他のテストや実行環境からの漏れ込みを断つ。
	for _, k := range []string{
		"APP_ENV", "APP_BASE_URL", "PORT", "CACHE_ENABLED", "LLM_ENABLED",
		"GEMINI_API_KEY", "GEMINI_MODEL", "RAKUTEN_AFFILIATE_ID", "PHOTO_PROXY_BASE_URL",
		"PLAN_MAX_CANDIDATES", "PLAN_TTL", "CORS_ORIGINS", "SERVER_READ_TIMEOUT",
		"ANTHROPIC_MODEL", "LLM_MAX_REPAIR_ATTEMPTS", "RAKUTEN_QPS",
	} {
		t.Setenv(k, "")
	}
}

func problems(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("エラーになるはずが nil でした")
	}
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("*config.Error ではありません: %T", err)
	}
	return strings.Join(e.Problems, "\n")
}

func TestLoadDefaults(t *testing.T) {
	setMinimum(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("最小構成で起動できません: %v", err)
	}
	if cfg.App.Env != EnvLocal || cfg.Server.Port != 8080 {
		t.Errorf("既定値が入っていません: env=%q port=%d", cfg.App.Env, cfg.Server.Port)
	}
	if cfg.Plan.TTL != 15*time.Minute {
		t.Errorf("PLAN_TTL = %v, want 15m（model.PlanTTL と一致すべき）", cfg.Plan.TTL)
	}
	if cfg.Plan.MaxCandidatesPerCategory != 8 {
		t.Errorf("PLAN_MAX_CANDIDATES = %d, want 8", cfg.Plan.MaxCandidatesPerCategory)
	}
	if cfg.LLM.Anthropic.Model != "claude-opus-5" || cfg.LLM.Anthropic.MaxTokens != 8000 {
		t.Errorf("LLM の既定値が違います: %+v", cfg.LLM.Anthropic)
	}
	if cfg.LLM.MaxRepairAttempts != 1 {
		t.Errorf("再生成回数 = %d, want 1", cfg.LLM.MaxRepairAttempts)
	}
	if !cfg.Cache.Enabled {
		t.Error("キャッシュは既定で有効であるべきです（Places/Routes の課金対策）")
	}
	if cfg.Rakuten.QPS != 1.0 {
		t.Errorf("RAKUTEN_QPS = %v, want 1", cfg.Rakuten.QPS)
	}
}

func TestMissingRequiredKeysAreReportedTogether(t *testing.T) {
	// 1 個ずつ落として直す往復を避けるため、欠落は全部まとめて報告する。
	setMinimum(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("GOOGLE_MAPS_API_KEY", "")
	t.Setenv("RAKUTEN_APPLICATION_ID", "")

	_, err := Load()
	got := problems(t, err)
	for _, want := range []string{"DATABASE_URL", "GOOGLE_MAPS_API_KEY", "RAKUTEN_APPLICATION_ID"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s の欠落が報告されていません:\n%s", want, got)
		}
	}
	if n := len(strings.Split(got, "\n")); n != 3 {
		t.Errorf("報告件数 = %d, want 3:\n%s", n, got)
	}
}

func TestSecretsAreRedacted(t *testing.T) {
	setMinimum(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("読み込みに失敗: %v", err)
	}
	// 設定をまるごとログに出しても鍵が漏れないこと。
	dump := fmt.Sprintf("%+v", cfg)
	for _, secret := range []string{"dummy-google-key", "dummy-rakuten-id", "dummy-anthropic-key", "postgres://"} {
		if strings.Contains(dump, secret) {
			t.Errorf("秘密情報が出力に漏れています: %q", secret)
		}
	}
	// 実際の値は Value() でだけ取れる。
	if cfg.Google.MapsAPIKey.Value() != "dummy-google-key" {
		t.Error("Value() で実値を取り出せません")
	}
	if Secret("").String() != "(未設定)" {
		t.Error("未設定の表示が違います")
	}
}

func TestLLMKeyRequirement(t *testing.T) {
	// LLM を使うなら提供元が最低 1 つ要る。
	setMinimum(t)
	t.Setenv("ANTHROPIC_API_KEY", "")

	if _, err := Load(); !strings.Contains(problems(t, err), "ANTHROPIC_API_KEY / GEMINI_API_KEY") {
		t.Error("LLM のキー欠落が報告されていません")
	}

	// Gemini だけでも成立する（フォールバック運用）。
	t.Setenv("GEMINI_API_KEY", "dummy-gemini-key")
	t.Setenv("GEMINI_MODEL", "gemini-x")
	if _, err := Load(); err != nil {
		t.Errorf("Gemini のみの構成が弾かれました: %v", err)
	}

	// モデル名を明示しない Gemini は許さない（存在しない型番を叩かせないため）。
	t.Setenv("GEMINI_MODEL", "")
	if _, err := Load(); !strings.Contains(problems(t, err), "GEMINI_MODEL") {
		t.Error("GEMINI_MODEL の欠落が報告されていません")
	}

	// LLM 自体を切れば鍵は不要。文章生成なしでもサービスは動く。
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("LLM_ENABLED", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("LLM 無効時に起動できません: %v", err)
	}
	if cfg.LLM.Enabled {
		t.Error("LLM_ENABLED=false が反映されていません")
	}
}

func TestCacheDisabledDropsRedisRequirement(t *testing.T) {
	setMinimum(t)
	t.Setenv("REDIS_URL", "")
	if _, err := Load(); !strings.Contains(problems(t, err), "REDIS_URL") {
		t.Error("キャッシュ有効時に REDIS_URL の欠落が報告されていません")
	}

	t.Setenv("CACHE_ENABLED", "false")
	if _, err := Load(); err != nil {
		t.Errorf("キャッシュ無効なら REDIS_URL は不要のはずです: %v", err)
	}
}

func TestProductionIsStricter(t *testing.T) {
	setMinimum(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("APP_BASE_URL", "http://lifestyle-mapper.app")
	t.Setenv("CORS_ORIGINS", "*")

	got := problems(t, mustFail(t))
	// 未設定だと収益がゼロになる項目は本番で起動を止める。
	for _, want := range []string{"RAKUTEN_AFFILIATE_ID", "PHOTO_PROXY_BASE_URL", "CORS_ORIGINS", "APP_BASE_URL"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s が本番で必須になっていません:\n%s", want, got)
		}
	}

	t.Setenv("RAKUTEN_AFFILIATE_ID", "affiliate-id")
	t.Setenv("PHOTO_PROXY_BASE_URL", "https://cdn.lifestyle-mapper.app")
	t.Setenv("APP_BASE_URL", "https://lifestyle-mapper.app")
	t.Setenv("CORS_ORIGINS", "https://lifestyle-mapper.app")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("整った本番構成が弾かれました: %v", err)
	}
	if !cfg.App.Env.IsProduction() {
		t.Error("本番判定が偽です")
	}
	// 末尾スラッシュは剥がして正規化する（shareUrl の // を防ぐ）。
	t.Setenv("APP_BASE_URL", "https://lifestyle-mapper.app/")
	cfg, _ = Load()
	if strings.HasSuffix(cfg.App.BaseURL, "/") {
		t.Errorf("BaseURL の末尾スラッシュが残っています: %q", cfg.App.BaseURL)
	}
}

func TestMalformedValues(t *testing.T) {
	setMinimum(t)
	t.Setenv("PORT", "八千八十")
	t.Setenv("SERVER_READ_TIMEOUT", "10")
	t.Setenv("CACHE_ENABLED", "yes-please")
	t.Setenv("PLAN_MAX_CANDIDATES", "99")
	t.Setenv("APP_ENV", "prod")

	got := problems(t, mustFail(t))
	for _, want := range []string{"PORT", "SERVER_READ_TIMEOUT", "CACHE_ENABLED", "PLAN_MAX_CANDIDATES", "APP_ENV"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s の不正が報告されていません:\n%s", want, got)
		}
	}
}

func mustFail(t *testing.T) error {
	t.Helper()
	_, err := Load()
	if err == nil {
		t.Fatal("エラーになるはずが成功しました")
	}
	return err
}
