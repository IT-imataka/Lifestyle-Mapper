package llm

import (
	"strings"
	"testing"

	"github.com/taka/lifestyle-mapper/backend/internal/apperror"
	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

func ptr(s string) *string { return &s }

// 設計書 2-B-1 の例に相当する、合格する下書き。
func validDraft() *PlanDraft {
	return &PlanDraft{
		PlanTitle:   "終演の余韻を、水道橋の深夜に持ち帰る",
		PlanSummary: "退場の混雑を30分やり過ごしてから、深夜2時まで営業の居酒屋へ。",
		Vibe:        "relaxed_late_night",
		ClosingNote: ptr("歩く距離を最小にすることを優先して組みました。"),
		Steps: []DraftStep{
			{Order: 1, Kind: model.SegmentBuffer, StayMinutes: 30, Headline: "まずは動かず、余韻に浸る",
				Reason: ptr("東京ドームは規制退場で最大30分かかります。")},
			{Order: 2, Kind: model.SegmentMove, StayMinutes: 0, Headline: "水道橋方面へ徒歩で"},
			{Order: 3, Kind: model.SegmentDining, CandidateID: ptr("cand_dining_003"), StayMinutes: 90,
				Headline: "深夜まで開いている居酒屋へ", Tip: ptr("歩きながらの電話予約がおすすめです。")},
			{Order: 4, Kind: model.SegmentMove, StayMinutes: 0, Headline: "ホテルへ"},
			{Order: 5, Kind: model.SegmentLodging, CandidateID: ptr("cand_lodging_001"), StayMinutes: 0,
				Headline: "会場徒歩圏で、翌朝もゆっくり"},
		},
		UnusedCandidateNotes: []UnusedCandidateNote{
			{CandidateID: "cand_dining_007", Reason: "23時閉店で滞在30分未満になるため見送りました。"},
		},
	}
}

func TestValidDraftPasses(t *testing.T) {
	d := validDraft()
	if v := d.Violations(); len(v) != 0 {
		t.Fatalf("正しい下書きが弾かれました: %v", v)
	}
	if err := d.Validate(model.LLMAnthropic); err != nil {
		t.Fatalf("Validate が失敗: %v", err)
	}

	// 参照している候補が時系列順に取れること（FactStore 照合の入力になる）。
	got := d.ReferencedCandidates()
	want := []model.CandidateID{"cand_dining_003", "cand_lodging_001"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("ReferencedCandidates() = %v, want %v", got, want)
	}
}

func TestViolationsCoverSchemaUnenforceableRules(t *testing.T) {
	// 構造化出力の JSON Schema では強制できない制約を、ここで確実に捕まえる。
	cases := []struct {
		name   string
		mutate func(*PlanDraft)
		want   string
	}{
		{"滞在時間の上限超過", func(d *PlanDraft) { d.Steps[2].StayMinutes = 600 }, "stayMinutes"},
		{"滞在時間が負", func(d *PlanDraft) { d.Steps[2].StayMinutes = -10 }, "stayMinutes"},
		{"move に滞在時間", func(d *PlanDraft) { d.Steps[1].StayMinutes = 15 }, "move の stayMinutes"},
		{"lodging に滞在時間", func(d *PlanDraft) { d.Steps[4].StayMinutes = 60 }, "lodging の stayMinutes"},
		{"buffer に候補ID", func(d *PlanDraft) { d.Steps[0].CandidateID = ptr("cand_dining_001") }, "buffer に candidateId"},
		{"dining に候補IDなし", func(d *PlanDraft) { d.Steps[2].CandidateID = nil }, "dining には candidateId"},
		{"lodging に候補IDなし", func(d *PlanDraft) { d.Steps[4].CandidateID = ptr("") }, "lodging には candidateId"},
		{"候補IDの形式不正", func(d *PlanDraft) { d.Steps[2].CandidateID = ptr("cand_dining_3") }, "形式が不正"},
		{"order の重複", func(d *PlanDraft) { d.Steps[3].Order = 3 }, "order が重複"},
		{"order の範囲外", func(d *PlanDraft) { d.Steps[0].Order = 99 }, "order は 1〜"},
		{"未定義の kind", func(d *PlanDraft) { d.Steps[0].Kind = "shopping" }, "kind が未定義"},
		{"未定義の vibe", func(d *PlanDraft) { d.Vibe = "very_fun" }, "vibe が未定義"},
		{"タイトルが空", func(d *PlanDraft) { d.PlanTitle = "  " }, "planTitle が空"},
		{"見出しが空", func(d *PlanDraft) { d.Steps[0].Headline = "" }, "headline が空"},
		{"タイトルが契約超過", func(d *PlanDraft) { d.PlanTitle = strings.Repeat("あ", MaxTitleRunes+1) }, "planTitle が 61 文字"},
		{"見出しが契約超過", func(d *PlanDraft) { d.Steps[0].Headline = strings.Repeat("あ", MaxHeadlineRunes+1) }, "headline が 41 文字"},
		{"理由が契約超過", func(d *PlanDraft) { d.Steps[0].Reason = ptr(strings.Repeat("あ", MaxReasonRunes+1)) }, "reason が 201 文字"},
		{"不採用理由が多すぎる", func(d *PlanDraft) {
			d.UnusedCandidateNotes = []UnusedCandidateNote{
				{CandidateID: "cand_dining_004", Reason: "a"}, {CandidateID: "cand_dining_005", Reason: "a"},
				{CandidateID: "cand_dining_006", Reason: "a"}, {CandidateID: "cand_dining_007", Reason: "a"},
			}
		}, "3 件以内"},
		{"採用済みを不採用として挙げる", func(d *PlanDraft) {
			d.UnusedCandidateNotes[0].CandidateID = "cand_dining_003"
		}, "採用済み"},
		{"ステップが空", func(d *PlanDraft) { d.Steps = nil }, "steps が空"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDraft()
			tc.mutate(d)
			v := d.Violations()
			if len(v) == 0 {
				t.Fatal("違反を検出できていません")
			}
			if !strings.Contains(strings.Join(v, "\n"), tc.want) {
				t.Errorf("期待した指摘がありません（want %q）:\n%s", tc.want, strings.Join(v, "\n"))
			}
		})
	}
}

func TestDuplicateCandidateIsRejected(t *testing.T) {
	// 同じ店に 2 回行くプランは提示しない。
	d := validDraft()
	d.Steps[4] = DraftStep{Order: 5, Kind: model.SegmentDining, CandidateID: ptr("cand_dining_003"),
		StayMinutes: 30, Headline: "もう一度同じ店へ"}
	v := strings.Join(d.Violations(), "\n")
	if !strings.Contains(v, "既に使われています") {
		t.Errorf("候補の重複を検出できていません:\n%s", v)
	}
}

func TestMoveStructureRules(t *testing.T) {
	// move は 2 つの滞在ステップの間にしか置けない。
	t.Run("先頭が move", func(t *testing.T) {
		d := validDraft()
		d.Steps[0].Kind = model.SegmentMove
		d.Steps[0].StayMinutes = 0
		if !strings.Contains(strings.Join(d.Violations(), "\n"), "先頭を move") {
			t.Error("先頭の move を検出できていません")
		}
	})
	t.Run("末尾が move", func(t *testing.T) {
		d := validDraft()
		d.Steps = d.Steps[:4] // 末尾が move になる
		if !strings.Contains(strings.Join(d.Violations(), "\n"), "末尾を move") {
			t.Error("末尾の move を検出できていません")
		}
	})
	t.Run("move の連続", func(t *testing.T) {
		d := validDraft()
		d.Steps[2] = DraftStep{Order: 3, Kind: model.SegmentMove, StayMinutes: 0, Headline: "さらに移動"}
		if !strings.Contains(strings.Join(d.Violations(), "\n"), "move が連続") {
			t.Error("move の連続を検出できていません")
		}
	})
}

func TestValidateReturnsRepairableError(t *testing.T) {
	d := validDraft()
	d.Steps[2].CandidateID = nil

	err := d.Validate(model.LLMAnthropic)
	if err == nil {
		t.Fatal("不正な下書きを通しました")
	}
	e := apperror.From(err)
	if e.Code != apperror.CodeLLMInvalidOutput {
		t.Errorf("Code = %q, want %q", e.Code, apperror.CodeLLMInvalidOutput)
	}
	// 組み直しで回復しうる、と validator が判断できること。
	if !e.Repairable() {
		t.Error("LLM_INVALID_OUTPUT は組み直し可能であるべきです")
	}
	// ユーザーには内部の指摘が漏れないこと。
	if strings.Contains(e.Message, "candidateId") {
		t.Errorf("内部の指摘がユーザー向けメッセージに漏れています: %q", e.Message)
	}
	// 一方で再生成プロンプト用には読める形で残っていること。
	if !strings.Contains(e.Error(), "candidateId") {
		t.Error("原因側に指摘が残っていません")
	}
}

func TestParseDraft(t *testing.T) {
	raw := []byte(`{
	  "planTitle":"深夜の水道橋へ","planSummary":"混雑を避けて移動します。","vibe":"relaxed_late_night",
	  "closingNote":null,
	  "steps":[{"order":1,"kind":"buffer","candidateId":null,"stayMinutes":30,
	            "headline":"余韻に浸る","reason":null,"tip":null}],
	  "unusedCandidateNotes":[]
	}`)
	d, err := ParseDraft(raw)
	if err != nil {
		t.Fatalf("解釈に失敗: %v", err)
	}
	if d.PlanTitle != "深夜の水道橋へ" || len(d.Steps) != 1 {
		t.Errorf("解釈結果が違います: %+v", d)
	}
	if d.ClosingNote != nil || d.Steps[0].CandidateID != nil {
		t.Error("null がポインタの nil になっていません")
	}
	if d.Steps[0].Candidate() != "" {
		t.Error("null の candidateId が空文字にならず返りました")
	}

	// additionalProperties:false を指定している以上、未知のキーは提供元の挙動変化の合図。
	if _, err := ParseDraft([]byte(`{"planTitle":"x","surprise":1}`)); err == nil {
		t.Error("未知のフィールドを黙って捨てました")
	}
	if _, err := ParseDraft([]byte(`{"planTitle":`)); err == nil {
		t.Error("壊れた JSON を通しました")
	}
	// 前置きの文章が混ざった応答も弾く。
	if _, err := ParseDraft([]byte(`{"planTitle":"x"} といった内容です`)); err == nil {
		t.Error("JSON の後ろの余分なデータを見逃しました")
	}
}

func TestStepsInOrderRestoresSequence(t *testing.T) {
	d := validDraft()
	d.Steps[0], d.Steps[4] = d.Steps[4], d.Steps[0] // 配列順を崩す

	ordered := d.StepsInOrder()
	for i, s := range ordered {
		if s.Order != i+1 {
			t.Errorf("%d 番目の order = %d, want %d", i, s.Order, i+1)
		}
	}
	// 元の配列は変更されないこと。
	if d.Steps[0].Order != 5 {
		t.Error("StepsInOrder が元のスライスを破壊しました")
	}
}

func TestNarrativeExtraction(t *testing.T) {
	d := validDraft()
	n := d.Narrative()
	if n.Title != d.PlanTitle || n.Vibe != "relaxed_late_night" {
		t.Errorf("プランの文章が移せていません: %+v", n)
	}
	sn := d.Steps[2].Narrative()
	if sn.Headline != "深夜まで開いている居酒屋へ" || sn.Reason != "" || sn.Tip == "" {
		t.Errorf("ステップの文章が移せていません: %+v", sn)
	}
	// closingNote が null でも落ちないこと。
	d.ClosingNote = nil
	if got := d.Narrative().ClosingNote; got != "" {
		t.Errorf("ClosingNote = %q, want 空文字", got)
	}
}
