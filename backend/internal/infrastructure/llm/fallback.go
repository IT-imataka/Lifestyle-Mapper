package llm

import (
	"context"
	"errors"
	"fmt"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Chain は提供元を順に試すフォールバック連鎖（Claude → Gemini）。
//
// 個人開発で一番痛いのは**片方が落ちた時に手が止まる**ことなので、
// 到達できなかった提供元は黙って次に譲る。ただし譲るのは LLM_UNAVAILABLE
// （接続・認証・レート制限・タイムアウト）のときだけ。
//
// LLM_INVALID_OUTPUT は「応答は得られたが中身が契約に合わない」であり、
// 提供元を替えても直る保証がない。こちらは呼び出し側の組み直し（repair）の担当で、
// ここで別の提供元を試すと**課金だけ倍になって同じ結果**になりかねない。
//
// 連鎖が全滅した場合は最後のエラーを返す。呼び出し側はさらにルールベースへ落ちる。
type Chain struct {
	composers []Composer
}

func NewChain(composers ...Composer) *Chain {
	// nil を混ぜて呼べるようにしておく（設定されていない提供元は nil で渡る）。
	live := make([]Composer, 0, len(composers))
	for _, c := range composers {
		if c != nil {
			live = append(live, c)
		}
	}
	return &Chain{composers: live}
}

// Len は実際に試せる提供元の数。0 なら LLM は使えない。
func (c *Chain) Len() int { return len(c.composers) }

// Provider は連鎖の先頭の提供元を返す。
// **実際に答えた提供元は Response.Meta.Provider を見ること**。
// この値は設定上の第一候補にすぎない。
func (c *Chain) Provider() model.LLMProvider {
	if len(c.composers) == 0 {
		return ""
	}
	return c.composers[0].Provider()
}

func (c *Chain) ComposePlan(ctx context.Context, req Request) (*Response, error) {
	if len(c.composers) == 0 {
		return nil, apperror.New(apperror.CodeLLMUnavailable, "").
			WithOp("llm.Chain").
			WithCause(errors.New("利用できる LLM の提供元がありません"))
	}

	var lastErr error
	for _, composer := range c.composers {
		// 打ち切りは連鎖全体の意思。次の提供元を試しても同じなので即座に返す。
		if err := ctx.Err(); err != nil {
			return nil, Unavailable(composer.Provider(), err)
		}

		resp, err := composer.ComposePlan(ctx, req)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if !apperror.HasCode(err, apperror.CodeLLMUnavailable) {
			// 応答は得られている。提供元を替えるのではなく組み直しで直す。
			return nil, err
		}
	}
	return nil, fmt.Errorf("すべての LLM 提供元に到達できませんでした: %w", lastErr)
}
