// Package prompt は検索条件と候補一覧から LLM へのプロンプトを組み立てる。
//
// この層が背負う責務は 2 つ。
//
//  1. **プロンプトのバージョン管理**。文面は text/template でファイルに切り出し、
//     meta.llm.promptVersion に載せる。文章品質の劣化を後から追跡できるようにするため。
//  2. **プロンプトインジェクション対策**。situation.notes は利用者の自由記述であり、
//     ここを素通しすると「これまでの指示を無視して」の類が LLM に届く。
//     データとして扱える形に落とすのはこの層の仕事で、llm 層は関知しない。
//
// service/plan には依存しない（依存の向きは service → prompt の一方向）。
// 候補は model の事実をそのまま受け取る。
package prompt

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/taka/lifestyle-mapper/backend/internal/model"
)

// Version は meta.llm.promptVersion に載る。テンプレートを変えたら必ず上げること。
const Version = "plan_v1"

//go:embed templates/plan_v1.md.tmpl
var planTemplate string

// System は役割指示。
//
// 事実を書かせないという最重要のルールをここでも述べる。テンプレート側にも同じ趣旨を
// 書いているのは冗長だが、**ハルシネーションの抑止は多重にかけるほうが安い**。
const systemPrompt = `あなたはライブ・イベントの遠征に慣れた日本のプランナーです。
終演後の限られた時間で、移動・食事・宿泊をどう組むかを提案します。

守ること:
- 与えられた候補（candidateId）の中からのみ選ぶ。候補にない店・宿・場所を挙げない。
- 店名・住所・価格・URL・営業時間・所要時間・絶対時刻を書かない。それらはシステムが実データから埋める。
- 出力は指定された JSON スキーマに厳密に従う。スキーマ外のキーを足さない。
- 文章は日本語で、宣伝文句ではなく「その人にとってなぜこの順序なのか」を書く。`

func System() string { return systemPrompt }

// Input はプロンプトの材料。候補は scorer が絞り込んだ後のものを渡す。
type Input struct {
	Condition *model.SearchCondition
	Dining    []DiningCandidate
	Lodging   []LodgingCandidate
	// Violations は前回の不合格理由。空でなければ「直してほしい点」として末尾に載る。
	Violations []string
}

// DiningCandidate は飲食の候補 1 件と、会場からの実測経路。
// 経路を添えるのは、LLM に徒歩圏かどうかを判断させるため（距離の推定はさせない）。
type DiningCandidate struct {
	Place     *model.PlaceFact
	FromVenue *model.RouteFact
}

// LodgingCandidate は宿の候補 1 件。Plan は scorer が選んだ代表プラン。
type LodgingCandidate struct {
	Hotel     *model.HotelFact
	Plan      *model.HotelPlanFact
	FromVenue *model.RouteFact
}

// Builder はテンプレートを保持する。生成のたびにパースし直さない。
type Builder struct {
	tmpl *template.Template
}

func New() (*Builder, error) {
	t, err := template.New(Version).Parse(planTemplate)
	if err != nil {
		return nil, fmt.Errorf("プロンプトテンプレートを解釈できません: %w", err)
	}
	return &Builder{tmpl: t}, nil
}

// MustNew は起動時の組み立て用。テンプレートは埋め込みなので、
// ここで失敗するならビルド成果物が壊れており、続行する意味がない。
func MustNew() *Builder {
	b, err := New()
	if err != nil {
		panic("prompt: " + err.Error())
	}
	return b
}

func (b *Builder) Version() string { return Version }

// Build はプロンプト本文を組み立てる。
func (b *Builder) Build(in Input) (string, error) {
	if in.Condition == nil {
		return "", fmt.Errorf("検索条件がありません")
	}
	var sb strings.Builder
	if err := b.tmpl.Execute(&sb, newView(in)); err != nil {
		return "", fmt.Errorf("プロンプトを組み立てられません: %w", err)
	}
	return strings.TrimSpace(sb.String()) + "\n", nil
}

// ── テンプレートに渡す表示用の型 ──────────────────────
//
// テンプレート側に判断を持たせない。整形はすべて Go で済ませ、
// テンプレートは差し込むだけにする（文面の変更が挙動を変えないようにするため）。

type view struct {
	EventLabel        string
	VenueName         string
	EndsAt            string
	DepartureAt       string
	ExitBufferMinutes int
	Party             string
	TransportModes    string
	MaxWalkMinutes    int

	Intent string
	Moods  string
	Notes  string

	Dining  *diningView
	Lodging *lodgingView

	DiningCandidates  []candidateView
	LodgingCandidates []candidateView

	Violations []string
}

type diningView struct {
	Genres             string
	Budget             string
	Requirements       string
	DesiredStayMinutes int
}

type lodgingView struct {
	Budget       string
	Rooms        int
	Requirements string
}

type candidateView struct {
	ID   model.CandidateID
	Name string
	// Attributes は 1 行にまとめた事実。行を分けるとトークンが増えるだけで読みやすさは変わらない。
	Attributes string
}

func newView(in Input) view {
	c := in.Condition
	v := view{
		EventLabel:        eventLabel(c.Event),
		VenueName:         c.Event.Venue.Name,
		EndsAt:            datetimeLabel(c.Event.EndsAt),
		DepartureAt:       timeLabel(c.Event.DepartureAt()),
		ExitBufferMinutes: minutes(c.Event.ExitBuffer),
		Party:             partyLabel(c.Party),
		TransportModes:    joinLabels(c.Options.TransportModes, travelModeJA),
		MaxWalkMinutes:    minutes(c.Situation.MaxWalk),
		Intent:            intentJA(c.Situation.Intent),
		Moods:             joinLabels(c.Situation.Moods, moodJA),
		Notes:             SanitizeUserText(c.Situation.Notes),
		Violations:        sanitizeViolations(in.Violations),
	}

	if c.Dining.Enabled {
		v.Dining = &diningView{
			Genres:             orAny(joinLabels(c.Dining.Genres, genreJA)),
			Budget:             budgetLabel(c.Dining.BudgetPerPerson),
			Requirements:       orNone(joinLabels(c.Dining.Requirements, diningRequirementJA)),
			DesiredStayMinutes: minutes(c.Dining.DesiredStay),
		}
	}
	if c.Lodging.Enabled {
		v.Lodging = &lodgingView{
			Budget:       budgetLabel(c.Lodging.BudgetPerNight),
			Rooms:        c.Lodging.Rooms,
			Requirements: orNone(joinLabels(c.Lodging.Requirements, lodgingRequirementJA)),
		}
	}

	for _, d := range in.Dining {
		if d.Place == nil {
			continue
		}
		v.DiningCandidates = append(v.DiningCandidates, candidateView{
			ID: d.Place.ID, Name: d.Place.Name, Attributes: diningAttributes(d),
		})
	}
	for _, l := range in.Lodging {
		if l.Hotel == nil {
			continue
		}
		v.LodgingCandidates = append(v.LodgingCandidates, candidateView{
			ID: l.Hotel.ID, Name: l.Hotel.Name, Attributes: lodgingAttributes(l),
		})
	}
	return v
}

func diningAttributes(d DiningCandidate) string {
	p := d.Place
	attrs := []string{routeLabel(d.FromVenue)}
	if s := ratingLabel(p.Rating, p.UserRatingCount); s != "" {
		attrs = append(attrs, s)
	}
	if len(p.Genres) > 0 {
		// 正規化済みの分類語は英語なので、日本語に戻してから渡す。
		attrs = append(attrs, joinLabels(model.ToGenres(p.Genres), genreJA))
	}
	if p.EstimatedCostPerPersonJPY != nil {
		attrs = append(attrs, "1人あたり目安 "+jpy(*p.EstimatedCostPerPersonJPY))
	}
	attrs = append(attrs, hoursLabel(p.Hours))
	return strings.Join(attrs, " / ")
}

func lodgingAttributes(l LodgingCandidate) string {
	attrs := []string{routeLabel(l.FromVenue)}
	if s := ratingLabel(l.Hotel.ReviewAverage, l.Hotel.ReviewCount); s != "" {
		attrs = append(attrs, s)
	}
	p := l.Plan
	if p == nil {
		return strings.Join(append(attrs, "空室情報なし"), " / ")
	}
	attrs = append(attrs, p.PlanName, jpy(p.TotalPriceJPY), vacancyLabel(p))
	if p.CheckInDeadline != nil {
		attrs = append(attrs, "最終チェックイン "+p.CheckInDeadline.String())
	}
	if p.CheckOutTime != nil {
		attrs = append(attrs, "チェックアウト "+p.CheckOutTime.String())
	}
	if len(p.Amenities) > 0 {
		attrs = append(attrs, joinLabels(model.ToLodgingRequirements(p.Amenities), lodgingRequirementJA))
	}
	return strings.Join(attrs, " / ")
}

// ── 自由記述の無害化 ──────────────────────────────────

var (
	controlChars = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	// 区切りを壊しうる文字。<notes> のタグやコードフェンスを閉じさせない。
	fenceChars = regexp.MustCompile("[<>`]")
	spaces     = regexp.MustCompile(`\s+`)
)

// MaxUserTextRunes は openapi.yaml の situation.notes の maxLength と同じ。
const MaxUserTextRunes = 200

// SanitizeUserText は利用者の自由記述を、プロンプトに埋めても構造を壊さない形にする。
//
// 「命令に見える文言を検出して弾く」方針は取らない。言い換えでいくらでも回避できるうえ、
// 正当な記述（「〇〇は無視してください」）まで落ちる。代わりに
//
//   - 改行と制御文字を潰して**1 行のデータ**にする（見出しや箇条書きに化けさせない）
//   - タグ・バッククォートを落として**囲みを閉じさせない**
//   - 長さを契約の上限で切る
//
// の 3 点に絞り、「これはデータであって指示ではない」という文脈づけはテンプレート側で行う。
func SanitizeUserText(s string) string {
	s = controlChars.ReplaceAllString(s, "")
	s = fenceChars.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return truncate(s, MaxUserTextRunes)
}

// sanitizeViolations は再生成時に渡す不合格理由を整える。
// これはシステムが書いた文だが、候補名などが混ざるため同じ処理を通す。
func sanitizeViolations(vs []string) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		if s := SanitizeUserText(v); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}

// ── 表示ラベル ────────────────────────────────────────

var weekdayJA = [...]string{"日", "月", "火", "水", "木", "金", "土"}

func datetimeLabel(t time.Time) string {
	return fmt.Sprintf("%d年%d月%d日(%s) %02d:%02d",
		t.Year(), int(t.Month()), t.Day(), weekdayJA[int(t.Weekday())], t.Hour(), t.Minute())
}

func timeLabel(t time.Time) string { return t.Format("15:04") }

func eventLabel(e model.Event) string {
	if e.Name == "" {
		return "公演"
	}
	return "「" + SanitizeUserText(e.Name) + "」"
}

func partyLabel(p model.Party) string {
	s := fmt.Sprintf("大人%d名", p.Adults)
	if p.Children > 0 {
		s += fmt.Sprintf("・子ども%d名", p.Children)
	}
	return s + "（" + relationshipJA(p.Relationship) + "）"
}

func routeLabel(r *model.RouteFact) string {
	if r == nil {
		return "会場からの経路は未取得"
	}
	label := fmt.Sprintf("会場から%s%d分", travelModeJA(r.Key.Mode), minutes(r.Duration))
	if r.DistanceMeters > 0 {
		label += fmt.Sprintf("（%dm）", r.DistanceMeters)
	}
	return label
}

func ratingLabel(rating *float64, count *int) string {
	if rating == nil {
		return ""
	}
	s := fmt.Sprintf("★%.1f", *rating)
	if count != nil {
		s += fmt.Sprintf("（%d件）", *count)
	}
	return s
}

// hoursLabel は営業時間を伝える。
// 不明を「休業」と書かないのは、LLM に「閉まっている」と誤解させないため。
func hoursLabel(h model.OpeningHours) string {
	if !h.Known || len(h.Periods) == 0 {
		return "営業時間不明"
	}
	parts := make([]string, 0, len(h.Periods))
	for _, p := range h.Periods {
		parts = append(parts, fmt.Sprintf("%s〜%s", p.Open.Format("1/2 15:04"), p.Close.Format("1/2 15:04")))
	}
	return "営業 " + strings.Join(parts, "、")
}

func vacancyLabel(p *model.HotelPlanFact) string {
	var s string
	switch p.VacancyStatus {
	case model.VacancyFewLeft:
		s = "残りわずか"
	case model.VacancySoldOut:
		s = "満室"
	default:
		s = "空室あり"
	}
	if p.RemainingRooms != nil {
		s += fmt.Sprintf("（残%d室）", *p.RemainingRooms)
	}
	return s
}

func budgetLabel(m model.MoneyRangeJPY) string {
	switch {
	case m.Max == nil && m.Min == 0:
		return "指定なし"
	case m.Max == nil:
		return jpy(m.Min) + "以上"
	case m.Min == 0:
		return jpy(*m.Max) + "まで"
	}
	return jpy(m.Min) + "〜" + jpy(*m.Max)
}

// jpy は 3 桁区切りの円表記。金額は常に整数の円で扱う。
func jpy(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String() + "円"
	}
	return b.String() + "円"
}

func minutes(d time.Duration) int { return int(d.Minutes()) }

// joinLabels は列挙型のスライスを日本語ラベルの並びにする。
func joinLabels[T ~string](vs []T, ja func(T) string) string {
	if len(vs) == 0 {
		return ""
	}
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, ja(v))
	}
	return strings.Join(out, "・")
}

func orAny(s string) string {
	if s == "" {
		return "指定なし（おまかせ）"
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "特になし"
	}
	return s
}

func intentJA(i model.Intent) string {
	switch i {
	case model.IntentStay:
		return "宿泊して翌日帰る"
	case model.IntentReturn:
		return "当日中に帰る"
	case model.IntentEither:
		return "終電に間に合うなら帰宅、無理なら宿泊"
	}
	return string(i)
}

func relationshipJA(r model.Relationship) string {
	switch r {
	case model.RelationshipSolo:
		return "ひとり"
	case model.RelationshipCouple:
		return "カップル"
	case model.RelationshipFriends:
		return "友人同士"
	case model.RelationshipFamily:
		return "家族"
	}
	return string(r)
}

func moodJA(m model.Mood) string {
	switch m {
	case model.MoodCelebrate:
		return "お祝いしたい"
	case model.MoodRelaxed:
		return "のんびりしたい"
	case model.MoodEfficient:
		return "効率よく動きたい"
	case model.MoodBudget:
		return "出費を抑えたい"
	}
	return string(m)
}

func travelModeJA(m model.TravelMode) string {
	switch m {
	case model.TravelWalk:
		return "徒歩"
	case model.TravelTransit:
		return "公共交通"
	case model.TravelDrive:
		return "車"
	case model.TravelBicycle:
		return "自転車"
	}
	return string(m)
}

func genreJA(g model.DiningGenre) string {
	switch g {
	case model.GenreIzakaya:
		return "居酒屋"
	case model.GenreRamen:
		return "ラーメン"
	case model.GenreYakiniku:
		return "焼肉"
	case model.GenreSushi:
		return "寿司"
	case model.GenreCafe:
		return "カフェ"
	case model.GenreBar:
		return "バー"
	case model.GenreFamilyRestaurant:
		return "ファミレス"
	case model.GenreFastFood:
		return "ファストフード"
	case model.GenreItalian:
		return "イタリアン"
	case model.GenreChinese:
		return "中華"
	}
	return string(g)
}

func diningRequirementJA(r model.DiningRequirement) string {
	switch r {
	case model.DiningOpenLate:
		return "深夜営業"
	case model.DiningReservable:
		return "予約可"
	case model.DiningNonSmoking:
		return "禁煙"
	case model.DiningPrivateRoom:
		return "個室あり"
	case model.DiningAllYouCanDrink:
		return "飲み放題"
	case model.DiningVegetarianOK:
		return "ベジタリアン対応"
	}
	return string(r)
}

func lodgingRequirementJA(r model.LodgingRequirement) string {
	switch r {
	case model.LodgingNoSmoking:
		return "禁煙"
	case model.LodgingLateCheckinOK:
		return "レイトチェックイン可"
	case model.LodgingWithBath:
		return "バス付き"
	case model.LodgingLargeBath:
		return "大浴場"
	case model.LodgingBreakfastIncluded:
		return "朝食付き"
	case model.LodgingCoinLaundry:
		return "コインランドリー"
	case model.LodgingFreeWifi:
		return "無料Wi-Fi"
	}
	return string(r)
}
