package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// PlanDraft は LLM に強制する中間契約（schemas/llm/plan_output.schema.json）の Go 表現。
// **フロントには出ない**。LLM が書いてよいのは candidateId の参照と日本語の文章だけで、
// 店名・住所・価格・URL・営業時間・絶対時刻は一切含まれない。
type PlanDraft struct {
	PlanTitle            string                `json:"planTitle"`
	PlanSummary          string                `json:"planSummary"`
	Vibe                 string                `json:"vibe"`
	Steps                []DraftStep           `json:"steps"`
	ClosingNote          *string               `json:"closingNote"`
	UnusedCandidateNotes []UnusedCandidateNote `json:"unusedCandidateNotes"`
}

// DraftStep は 1 ステップ。kind に応じて candidateId と stayMinutes の扱いが変わる。
type DraftStep struct {
	Order int `json:"order"`
	// Kind はドメインのセグメント種別と同じ語彙。ここが一致しているので
	// Hydrator は写像表を持たずに分岐できる。
	Kind        model.SegmentType `json:"kind"`
	CandidateID *string           `json:"candidateId"`
	StayMinutes int               `json:"stayMinutes"`
	Headline    string            `json:"headline"`
	Reason      *string           `json:"reason"`
	Tip         *string           `json:"tip"`
}

// Candidate は candidateId をドメインの型で返す。null / 空文字はどちらも「参照なし」。
func (s DraftStep) Candidate() model.CandidateID {
	if s.CandidateID == nil {
		return ""
	}
	return model.CandidateID(strings.TrimSpace(*s.CandidateID))
}

type UnusedCandidateNote struct {
	CandidateID string `json:"candidateId"`
	Reason      string `json:"reason"`
}

// 長さの上限は **API 契約（openapi.yaml）の maxLength** に合わせる。
//
// JSON Schema の構造化出力は maxLength / minimum を強制できないため、ここで検査する。
// なお LLM スキーマの description には「30文字以内」等のより厳しい目安が書いてあるが、
// あれは作文の指示であって契約ではない。数文字の超過で 5 秒かけて全体を再生成するのは
// 割に合わないので、**契約を破る長さだけを不合格**とする。
const (
	MaxTitleRunes       = 60
	MaxSummaryRunes     = 300
	MaxClosingNoteRunes = 300
	MaxVibeRunes        = 40
	MaxHeadlineRunes    = 40
	MaxReasonRunes      = 200
	MaxTipRunes         = 200
	MaxUnusedNotes      = 3
	MaxStayMinutes      = 240
)

// vibes は UI のテーマ色・アイコン選択に使うため、値が増えるとフロントの対応が要る。
var vibes = map[string]bool{
	"relaxed_late_night":   true,
	"efficient_transit":    true,
	"celebration":          true,
	"budget_conscious":     true,
	"sightseeing_extended": true,
	"quick_return":         true,
}

// ParseDraft は LLM の応答 JSON を解釈する。
//
// 未知のフィールドを拒否するのは、スキーマ側で additionalProperties:false を
// 指定している以上、余計なキーが来た時点で提供元の挙動が変わった合図だから。
// 黙って捨てると気づけない。
func ParseDraft(raw []byte) (*PlanDraft, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var d PlanDraft
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("構造化出力を解釈できません: %w", err)
	}
	if dec.More() {
		return nil, errors.New("構造化出力の後ろに余分なデータがあります")
	}
	return &d, nil
}

// StepsInOrder は order 昇順のステップを返す。
// 配列順が入れ替わっていても時系列を復元できるようにしておく。
func (d *PlanDraft) StepsInOrder() []DraftStep {
	out := append([]DraftStep(nil), d.Steps...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// ReferencedCandidates は下書きが参照している候補 ID を時系列順に返す。
// FactStore との照合（Hydrator の防壁）の入力になる。
func (d *PlanDraft) ReferencedCandidates() []model.CandidateID {
	var ids []model.CandidateID
	for _, s := range d.StepsInOrder() {
		if id := s.Candidate(); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// Violations は構造化出力のスキーマでは守れない制約の違反を列挙する。
//
// 戻り値をそのまま再生成プロンプトに載せる想定なので、
// **LLM が読んで直せる日本語**で書くこと（ユーザー向けの文言ではない）。
func (d *PlanDraft) Violations() []string {
	var v []string
	add := func(format string, args ...any) { v = append(v, fmt.Sprintf(format, args...)) }

	// ── 全体 ──
	if strings.TrimSpace(d.PlanTitle) == "" {
		add("planTitle が空です")
	}
	if n := utf8.RuneCountInString(d.PlanTitle); n > MaxTitleRunes {
		add("planTitle が %d 文字です。%d 文字以内にしてください", n, MaxTitleRunes)
	}
	if strings.TrimSpace(d.PlanSummary) == "" {
		add("planSummary が空です")
	}
	if n := utf8.RuneCountInString(d.PlanSummary); n > MaxSummaryRunes {
		add("planSummary が %d 文字です。%d 文字以内にしてください", n, MaxSummaryRunes)
	}
	if d.ClosingNote != nil {
		if n := utf8.RuneCountInString(*d.ClosingNote); n > MaxClosingNoteRunes {
			add("closingNote が %d 文字です。%d 文字以内にしてください", n, MaxClosingNoteRunes)
		}
	}
	if !vibes[d.Vibe] {
		add("vibe が未定義の値です: %q。指定された選択肢から選んでください", d.Vibe)
	}
	if utf8.RuneCountInString(d.Vibe) > MaxVibeRunes {
		add("vibe が長すぎます")
	}

	// ── ステップ ──
	if len(d.Steps) == 0 {
		return append(v, "steps が空です。少なくとも 1 つのステップが必要です")
	}

	seenOrder := make(map[int]bool, len(d.Steps))
	seenCandidate := make(map[model.CandidateID]int, len(d.Steps))
	for _, s := range d.Steps {
		label := fmt.Sprintf("order=%d", s.Order)

		// order は 1 起点の連番。欠番・重複があると時系列が確定しない。
		if s.Order < 1 || s.Order > len(d.Steps) {
			add("%s: order は 1〜%d の範囲で指定してください", label, len(d.Steps))
		} else if seenOrder[s.Order] {
			add("%s: order が重複しています", label)
		}
		seenOrder[s.Order] = true

		if n := utf8.RuneCountInString(s.Headline); n == 0 {
			add("%s: headline が空です", label)
		} else if n > MaxHeadlineRunes {
			add("%s: headline が %d 文字です。%d 文字以内にしてください", label, n, MaxHeadlineRunes)
		}
		if s.Reason != nil {
			if n := utf8.RuneCountInString(*s.Reason); n > MaxReasonRunes {
				add("%s: reason が %d 文字です。%d 文字以内にしてください", label, n, MaxReasonRunes)
			}
		}
		if s.Tip != nil {
			if n := utf8.RuneCountInString(*s.Tip); n > MaxTipRunes {
				add("%s: tip が %d 文字です。%d 文字以内にしてください", label, n, MaxTipRunes)
			}
		}

		if s.StayMinutes < 0 || s.StayMinutes > MaxStayMinutes {
			add("%s: stayMinutes は 0〜%d で指定してください（現在: %d）", label, MaxStayMinutes, s.StayMinutes)
		}

		id := s.Candidate()
		switch s.Kind {
		case model.SegmentBuffer:
			if id != "" {
				add("%s: buffer に candidateId は指定できません。null にしてください", label)
			}
		case model.SegmentMove:
			if id != "" {
				add("%s: move に candidateId は指定できません。null にしてください", label)
			}
			// 移動時間はシステムが実測値で埋めるため、ここでの提案値は使わない。
			if s.StayMinutes != 0 {
				add("%s: move の stayMinutes は 0 にしてください（移動時間はシステムが算出します）", label)
			}
		case model.SegmentDining:
			if id == "" {
				add("%s: dining には candidateId が必要です", label)
			}
		case model.SegmentLodging:
			if id == "" {
				add("%s: lodging には candidateId が必要です", label)
			}
			// 宿泊は翌朝までなので滞在時間の概念を持たせない。
			if s.StayMinutes != 0 {
				add("%s: lodging の stayMinutes は 0 にしてください", label)
			}
		default:
			add("%s: kind が未定義の値です: %q", label, s.Kind)
		}

		if id != "" {
			// 形式だけ見る。実在するかの照合は FactStore の仕事。
			if !id.Valid() {
				add("%s: candidateId の形式が不正です: %q", label, id)
			}
			if prev, dup := seenCandidate[id]; dup {
				add("%s: %s は order=%d で既に使われています。同じ候補は 1 回だけ使ってください", label, id, prev)
			} else {
				seenCandidate[id] = s.Order
			}
		}
	}

	// move が先頭・末尾に来ると「どこからどこへ」が定まらない。
	ordered := d.StepsInOrder()
	if ordered[0].Kind == model.SegmentMove {
		add("先頭を move にはできません。会場からの移動は 2 番目以降に置いてください")
	}
	if last := ordered[len(ordered)-1]; last.Kind == model.SegmentMove {
		add("末尾を move にはできません。移動の行き先となるステップが必要です")
	}
	for i := 1; i < len(ordered); i++ {
		if ordered[i].Kind == model.SegmentMove && ordered[i-1].Kind == model.SegmentMove {
			add("order=%d: move が連続しています。移動の間には滞在するステップが必要です", ordered[i].Order)
		}
	}

	// ── 不採用理由 ──
	if len(d.UnusedCandidateNotes) > MaxUnusedNotes {
		add("unusedCandidateNotes は %d 件以内にしてください（現在: %d 件）", MaxUnusedNotes, len(d.UnusedCandidateNotes))
	}
	for _, n := range d.UnusedCandidateNotes {
		id := model.CandidateID(strings.TrimSpace(n.CandidateID))
		if !id.Valid() {
			add("unusedCandidateNotes: candidateId の形式が不正です: %q", n.CandidateID)
		}
		if _, used := seenCandidate[id]; used {
			add("unusedCandidateNotes: %s は採用済みです。不採用の候補だけを挙げてください", id)
		}
		if strings.TrimSpace(n.Reason) == "" {
			add("unusedCandidateNotes: %s の reason が空です", id)
		}
	}

	return v
}

// Validate は Violations が空であることを確かめる。
// 返るエラーは LLM_INVALID_OUTPUT（= Repairable）なので、
// 呼び出し側は Violations() を再生成プロンプトに載せて 1 回だけ組み直させられる。
func (d *PlanDraft) Validate(provider model.LLMProvider) error {
	v := d.Violations()
	if len(v) == 0 {
		return nil
	}
	return InvalidOutput(provider, fmt.Errorf("スキーマ適合外の出力です: %s", strings.Join(v, " / ")))
}

// Narrative は下書きの文章部分をドメインの型に移す。事実は一切含まない。
func (d *PlanDraft) Narrative() *model.PlanNarrative {
	n := &model.PlanNarrative{
		Title:   strings.TrimSpace(d.PlanTitle),
		Summary: strings.TrimSpace(d.PlanSummary),
		Vibe:    d.Vibe,
	}
	if d.ClosingNote != nil {
		n.ClosingNote = strings.TrimSpace(*d.ClosingNote)
	}
	return n
}

// Narrative はステップの文章部分をドメインの型に移す。
func (s DraftStep) Narrative() *model.SegmentNarrative {
	n := &model.SegmentNarrative{Headline: strings.TrimSpace(s.Headline)}
	if s.Reason != nil {
		n.Reason = strings.TrimSpace(*s.Reason)
	}
	if s.Tip != nil {
		n.Tip = strings.TrimSpace(*s.Tip)
	}
	return n
}
