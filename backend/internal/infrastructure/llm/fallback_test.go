package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

type stubComposer struct {
	provider model.LLMProvider
	resp     *Response
	err      error
	calls    int
}

func (s *stubComposer) ComposePlan(_ context.Context, _ Request) (*Response, error) {
	s.calls++
	return s.resp, s.err
}

func (s *stubComposer) Provider() model.LLMProvider { return s.provider }

func ok(p model.LLMProvider) *stubComposer {
	return &stubComposer{provider: p, resp: &Response{Draft: validDraft(), Meta: model.LLMMeta{Provider: p}}}
}

func down(p model.LLMProvider) *stubComposer {
	return &stubComposer{provider: p, err: Unavailable(p, errors.New("接続できません"))}
}

func broken(p model.LLMProvider) *stubComposer {
	return &stubComposer{provider: p, err: InvalidOutput(p, errors.New("スキーマ違反"))}
}

func TestChainUsesFirstAvailable(t *testing.T) {
	first, second := ok(model.LLMAnthropic), ok(model.LLMGoogle)

	resp, err := NewChain(first, second).ComposePlan(context.Background(), Request{})
	if err != nil {
		t.Fatalf("ComposePlan: %v", err)
	}
	if resp.Meta.Provider != model.LLMAnthropic {
		t.Errorf("応答したのは %s です（anthropic を期待）", resp.Meta.Provider)
	}
	if second.calls != 0 {
		t.Error("先頭が成功したのに次の提供元も呼ばれています")
	}
}

// 到達できない提供元は黙って次に譲る。片方が落ちても手が止まらないのが目的。
func TestChainFallsThroughOnUnavailable(t *testing.T) {
	first, second := down(model.LLMAnthropic), ok(model.LLMGoogle)

	resp, err := NewChain(first, second).ComposePlan(context.Background(), Request{})
	if err != nil {
		t.Fatalf("ComposePlan: %v", err)
	}
	if resp.Meta.Provider != model.LLMGoogle {
		t.Errorf("応答したのは %s です（google を期待）", resp.Meta.Provider)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Errorf("呼び出し回数が %d / %d です（1 / 1 を期待）", first.calls, second.calls)
	}
}

// 中身が契約に合わないのは提供元を替えても直らない。課金を倍にせず組み直しに回す。
func TestChainDoesNotFallThroughOnInvalidOutput(t *testing.T) {
	first, second := broken(model.LLMAnthropic), ok(model.LLMGoogle)

	_, err := NewChain(first, second).ComposePlan(context.Background(), Request{})
	if err == nil {
		t.Fatal("スキーマ違反が握り潰されました")
	}
	if got := apperror.CodeOf(err); got != apperror.CodeLLMInvalidOutput {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeLLMInvalidOutput)
	}
	if second.calls != 0 {
		t.Error("組み直しで直る失敗なのに次の提供元が呼ばれています")
	}
}

func TestChainReportsUnavailableWhenAllFail(t *testing.T) {
	_, err := NewChain(down(model.LLMAnthropic), down(model.LLMGoogle)).
		ComposePlan(context.Background(), Request{})
	if err == nil {
		t.Fatal("全滅なのにエラーが返りません")
	}
	// ルールベースへ落とす判断ができるよう、コードは維持されている。
	if !apperror.HasCode(err, apperror.CodeLLMUnavailable) {
		t.Errorf("LLM_UNAVAILABLE が伝わっていません: %v", err)
	}
}

func TestChainIgnoresUnconfiguredProviders(t *testing.T) {
	chain := NewChain(nil, ok(model.LLMGoogle), nil)
	if chain.Len() != 1 {
		t.Fatalf("試せる提供元が %d 件です（1 件を期待）", chain.Len())
	}
	if chain.Provider() != model.LLMGoogle {
		t.Errorf("先頭が %s です（google を期待）", chain.Provider())
	}
}

func TestEmptyChainIsUnavailable(t *testing.T) {
	_, err := NewChain().ComposePlan(context.Background(), Request{})
	if got := apperror.CodeOf(err); got != apperror.CodeLLMUnavailable {
		t.Errorf("エラーコードが %s です（%s を期待）", got, apperror.CodeLLMUnavailable)
	}
}

// 打ち切りは連鎖全体の意思。次を試しても同じなので即座に返す。
func TestChainStopsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	first := ok(model.LLMAnthropic)

	_, err := NewChain(first).ComposePlan(ctx, Request{})
	if err == nil {
		t.Fatal("打ち切られたのに応答が返りました")
	}
	if first.calls != 0 {
		t.Error("打ち切り後に提供元が呼ばれています")
	}
}
