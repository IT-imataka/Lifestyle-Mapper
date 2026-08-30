// Package llm は「プロンプトを渡すと構造化された PlanDraft が返る」という
// 一点だけを抽象化する。Claude / Gemini を差し替え可能にするのが目的。
//
// この層が知らないこと（意図的に知らせないこと）:
//   - 検索条件やユーザーの事情。プロンプトの組み立ては prompt/builder.go の責務。
//   - 候補が実在するか。candidateId の照合は FactStore と Hydrator の責務。
//     ここは「LLM がスキーマどおりの JSON を返したか」までしか保証しない。
//
// JSON タグを持つのはこの層が外部表現の受け皿だから。model 側の禁止則とは対象が違う。
package llm

import (
	"context"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Composer は構造化出力でプランの下書きを書かせる。
//
// 設計書の `ComposePlan(ctx, in) (*PlanDraft, error)` に対し、戻り値を Response に
// 広げている。meta.llm（どの提供元が実際に答えたか・トークン数・レイテンシ）を
// 呼び出し側が組み立てられないと、フォールバック時に観測が途切れるため。
type Composer interface {
	// ComposePlan はプロンプトを投げ、スキーマ適合を検証済みの下書きを返す。
	//
	// 返すエラーは必ず *apperror.Error で、次のいずれか:
	//   - LLM_UNAVAILABLE   : 接続・認証・レート制限・タイムアウト（提供元を替えれば回復しうる）
	//   - LLM_INVALID_OUTPUT: JSON として壊れている / スキーマに適合しない（組み直しで回復しうる）
	ComposePlan(ctx context.Context, req Request) (*Response, error)

	// Provider は meta.llm に載る提供元。
	Provider() model.LLMProvider
}

// Request は 1 回の生成依頼。
type Request struct {
	// Prompt は prompt/builder.go が展開済みのユーザーメッセージ。
	// ユーザーの自由記述（situation.notes）が埋まるため、
	// **インジェクション対策は builder 側で済ませてから渡すこと**。
	Prompt string

	// System は役割指示。空なら提供元の既定に任せる。
	System string

	// Schema は構造化出力に強制する JSON Schema。空なら埋め込みの既定スキーマを使う。
	Schema []byte

	// MaxTokens / Effort は config から渡る。Effort は Anthropic のみ意味を持つ。
	MaxTokens int
	Effort    string

	// Violations は再生成時に前回の不合格理由を伝えるためのもの。
	// 空でなければ builder が「前回はここが駄目だった」という文をプロンプトに足す。
	Violations []string
}

// Response は生成結果と、その 1 回ぶんの観測情報。
type Response struct {
	Draft *PlanDraft
	Meta  model.LLMMeta
	// Raw は提供元が返した生の JSON。障害調査用にログへ落とす。
	Raw []byte
}

// Unavailable は提供元に到達できなかった／応答を得られなかった場合のエラー。
// フォールバック連鎖はこのコードを見て次の提供元に進む。
func Unavailable(provider model.LLMProvider, cause error) *apperror.Error {
	return apperror.Wrap(cause, apperror.CodeLLMUnavailable, "llm:"+string(provider))
}

// InvalidOutput は応答は得られたが中身がスキーマに適合しない場合のエラー。
// 提供元を替えても直らないので、組み直し（repair）で回復を図る。
func InvalidOutput(provider model.LLMProvider, cause error) *apperror.Error {
	return apperror.Wrap(cause, apperror.CodeLLMInvalidOutput, "llm:"+string(provider))
}

// NewMeta は 1 回の呼び出し実績を組み立てる。RepairAttempts は
// 連鎖全体を数える composer が後から埋めるため、ここでは触らない。
func NewMeta(provider model.LLMProvider, modelName string, latency time.Duration, in, out int) model.LLMMeta {
	return model.LLMMeta{
		Provider:      provider,
		Model:         modelName,
		SchemaVersion: SchemaVersion,
		PromptVersion: "", // composer が config の値で埋める
		Latency:       latency,
		InputTokens:   in,
		OutputTokens:  out,
	}
}
