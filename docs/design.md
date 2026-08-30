# Lifestyle Mapper 設計書 (v2: LLMオーケストレーション版)

_作成: 2026-08-29_


---

# 0. 先に一点、データフローの修正提案

ご提示のフローのうち **⑤「LLM が返した予約 URL・システムデータ統合済み JSON をそのまま返す」** だけは、そのまま実装すると事故ります。

LLM に URL・価格・残室数を**書かせる**と、ハルシネーションで `https://hb.afl.rakuten.co.jp/...` の ID が 1 文字変わるだけで**アフィリエイト収益がゼロになり**、価格や空室数がずれれば景品表示法上の問題にもなります。加えて、Claude / Gemini の構造化出力は JSON Schema の `minimum` / `maxLength` 等の数値・長さ制約をサポートしないため、スキーマだけでは値の正しさを担保できません。

そこで **④と⑤の間に「Hydrator（再水和）」層**を挟みます。

| 決定主体 | 担当領域 |
|---|---|
| **LLM** | 候補の**選択**、**順序**、**滞在時間の提案**、キャッチコピー・選定理由・Tips（＝言語と判断） |
| **Go** | **絶対時刻**の計算、**移動時間**（Routes API 実測値）、**価格・残室数・営業時間**、**アフィリエイト URL**（＝事実） |

LLM の出力は「`candidateId` の参照＋文章」だけに絞り、Go が FactStore と突合して最終 JSON を組み立てます。**フロントに返る JSON の形はご提示のイメージ通り（統合済みの美しい JSON をそのまま返却）** で、統合の実行者が LLM ではなく Go になるだけです。以下、この前提で全体を再構築します。

```
[Next.js Form]
    │ ① POST /v1/plans  (SearchRequest JSON)
    ▼
[Go: Controller] ──▶ [Service: Collector]
                         │ ② errgroup で並行 fan-out
                         ├─▶ Rakuten Travel API   (空室検索)
                         ├─▶ Google Places API    (飲食店/スポット)
                         └─▶ Google Routes API    (RouteMatrix: 会場→全候補)
                         ▼
                     [Normalizer] ③ 生データ → 正規化ファクト
                         ▼
                     [Filter/Scorer] ③' 徒歩圏・営業時間・予算で足切り → 上位N件
                         ▼
                     [FactStore]  candidateId を採番して事実を保持 ★真実の源
                         ▼
                     [Composer]   ④ LLM (output_config.format = json_schema)
                         │            → 順序・滞在時間・文章のみ
                         ▼
                     [Hydrator]   ⑤ LLM出力 × FactStore → 絶対時刻/URL/価格を確定
                         ▼
                     [Validator]  ⑤' 時刻矛盾・不正ID・徒歩超過を検査（不合格は1回リトライ）
                         ▼
                     [View]       PlanResponse へ整形
    │
    ▼
[Next.js: TimelineView] ⑥ 描画
```

---

# 1. ディレクトリ構成

## 1-0. モノレポ全体

```
lifestyle-mapper/
├── frontend/                       # Next.js (App Router)
├── backend/                        # Go API (ECS Fargate)
├── infra/                          # Terraform
│   ├── modules/{network,ecs,rds,alb,secrets,cloudfront}/
│   └── envs/{dev,prod}/
├── schemas/                        # ★フロント・バック・LLM の共通契約
│   ├── openapi.yaml                #   API契約（TS型を自動生成）
│   └── llm/plan_output.schema.json #   LLM構造化出力スキーマ（Goが埋め込み読込）
├── docs/adr/
├── Makefile
└── docker-compose.yml              # api + postgres + redis + localstack
```

`schemas/` を独立させるのが要点です。**API 契約と LLM 契約の 2 本を版管理**し、TypeScript 型は `openapi.yaml` から生成、Go は `llm/plan_output.schema.json` を `go:embed` でそのまま API に送ります。スキーマの手書き二重管理をやめないと、LLM 層が入った時点で確実に破綻します。

## 1-1. バックエンド（Go / MVCS）

```
backend/
├── cmd/
│   ├── api/main.go                     # DI組み立て・HTTPサーバ起動のみ
│   └── worker/main.go                  # 非同期プラン生成ワーカー（SQS消費）
│
├── internal/
│   ├── config/
│   │   └── config.go                   # env読込 + 起動時バリデーション（APIキー欠落は即死）
│   ├── router/router.go
│   ├── middleware/                     # RequestID / Logger / Recover / CORS / RateLimit
│   │
│   ├── controller/                     # ★C: HTTP境界。ロジック禁止
│   │   ├── plan_controller.go          #   POST /v1/plans, GET /v1/plans/{id}
│   │   ├── stream_controller.go        #   GET /v1/plans/{id}/events (SSE)
│   │   ├── click_controller.go         #   POST /v1/clicks (アフィリ計測)
│   │   └── health_controller.go
│   │
│   ├── service/                        # ★S: ビジネスロジック（本体）
│   │   ├── plan/
│   │   │   ├── service.go              #   オーケストレータ（①〜⑦の司令塔）
│   │   │   ├── collector.go            #   ② errgroup で3API並行fan-out・部分失敗許容
│   │   │   ├── normalizer.go           #   ③ 各社の生レスポンス → model.Fact へ正規化
│   │   │   ├── scorer.go               #   ③' 徒歩/営業時間/予算/評価でスコア→上位N件
│   │   │   ├── factstore.go            #   candidateId 採番と事実の保管（★真実の源）
│   │   │   ├── composer.go             #   ④ プロンプト構築 → LLM構造化出力呼び出し → ルールベース素組み
│   │   │   ├── hydrator.go             #   ⑤ LLM出力 × FactStore → 確定モデル生成
│   │   │   ├── timecalc.go             #   ⑤' endsAt起点の絶対時刻累積計算
│   │   │   └── validator.go            #   ⑤'' 整合性検査 + リトライ判定
│   │   ├── hotel/service.go            #   楽天ドメインロジック（空室判定・料金計算）
│   │   ├── place/service.go            #   Places ドメインロジック（営業時間判定）
│   │   └── route/service.go            #   Routes ドメインロジック（徒歩/公共交通・終電）
│   │
│   ├── model/                          # ★M: ドメインモデル（JSONタグを持たない）
│   │   ├── search.go                   #   検索条件
│   │   ├── fact.go                     #   HotelFact / PlaceFact / RouteFact
│   │   ├── plan.go                     #   Plan / Segment / Timeline
│   │   └── event.go
│   │
│   ├── view/                           # ★V: レスポンスDTO（JSONタグはここだけ）
│   │   ├── envelope.go                 #   {data, meta} / {error}
│   │   ├── plan_view.go                #   model.Plan → PlanResponse
│   │   └── error_view.go
│   │
│   ├── repository/                     # RDS永続化（interfaceは service 側に定義）
│   │   ├── plan_repository.go          #   生成済みプランの保存（共有URL用）
│   │   ├── event_repository.go
│   │   └── click_repository.go
│   │
│   ├── infrastructure/                 # 外部依存の実装
│   │   ├── db/postgres.go
│   │   ├── cache/redis.go              #   ★必須: Places/Routes のコスト削減
│   │   ├── rakutentravel/
│   │   │   ├── client.go               #   VacantHotelSearch（1req/sec制限に注意）
│   │   │   └── dto.go
│   │   ├── googleplaces/
│   │   │   ├── client.go               #   Places API (New) searchNearby + FieldMask必須
│   │   │   └── dto.go
│   │   ├── googleroutes/
│   │   │   ├── client.go               #   computeRouteMatrix / computeRoutes
│   │   │   └── dto.go
│   │   └── llm/
│   │       ├── llm.go                  #   interface ComposePlan(ctx, in) (*PlanDraft, error)
│   │       ├── claude/client.go        #   output_config.format = json_schema
│   │       ├── gemini/client.go        #   responseSchema / responseMimeType
│   │       └── fallback.go             #   claude失敗 → gemini
│   │
│   ├── prompt/
│   │   ├── templates/plan_v1.md.tmpl   #   text/template（プロンプトもバージョン管理）
│   │   └── builder.go
│   │
│   └── apperror/errors.go              # ドメインエラー ⇔ HTTPステータス対応表
│
├── migrations/
├── testdata/                           # 各社APIのゴールデンレスポンス（LLMを叩かずテスト）
├── Dockerfile                          # multi-stage → distroless
└── go.mod
```

**この構成にした理由**

- `service/plan/` を **7 ファイルに分解**したのは、このアプリの複雑性がすべてここに集中するからです。`plan_service.go` 1 枚に押し込むと 1500 行超えて手が付けられなくなります。ファイル名がそのままパイプラインの段階名になっているので、障害時にどこを見るかが即座に分かります。
- **`factstore.go` が設計の心臓部**です。ここが `candidateId → 検証済み事実` の唯一の写像を持ち、Hydrator はここにない ID を弾きます。LLM の幻覚はここで物理的に遮断されます。
- `infrastructure/llm/llm.go` に interface を切り、Claude / Gemini を差し替え可能にします。個人開発では**片方が落ちた時に手が止まる**のが一番痛いので、`fallback.go` で「Claude → Gemini → ルールベース素組み（文章なしで時系列だけ返す）」の 3 段構えにしておくと、サービスが死にません。
- `controller` はリクエスト検証と `service` 呼び出しのみ。**`model` に JSON タグを一切付けない**ルールにより、DB カラムを足しても API に漏れません。

**外部 API 実装上の注意（先に潰しておくべき点）**

- **Google Places API (New)** は `X-Goog-FieldMask` 必須で、要求フィールドによって課金 SKU が変わります。`places.id,places.displayName,places.location,places.rating,places.currentOpeningHours` 程度に絞ってください。
- **Google Routes API** は候補ごとに `computeRoutes` を N 回叩くと課金が爆発します。**`computeRouteMatrix` で会場→全候補を 1 リクエスト**にまとめ、確定した経路だけ `computeRoutes` で詳細取得する 2 段構えにします。
- **楽天ウェブサービス**は概ね 1req/sec のレート制限があるため、`golang.org/x/time/rate` の Limiter を `rakutentravel/client.go` 内に持たせます。並行 fan-out といっても楽天だけは直列化が必要です。

## 1-2. フロントエンド（Next.js App Router）

```
frontend/
├── src/
│   ├── app/
│   │   ├── layout.tsx
│   │   ├── page.tsx                        # LP + 検索フォーム
│   │   ├── plan/
│   │   │   └── [planId]/
│   │   │       ├── page.tsx                # 結果（Server Component で初回取得）
│   │   │       ├── loading.tsx
│   │   │       ├── error.tsx
│   │   │       └── opengraph-image.tsx     # シェア用OGP（流入導線）
│   │   ├── api/                            # ★薄いBFFのみ。ロジック禁止
│   │   │   ├── plans/route.ts              #   Goへプロキシ（内部APIキーを秘匿）
│   │   │   └── clicks/route.ts             #   アフィリクリック計測をGoへ中継
│   │   └── globals.css
│   │
│   ├── features/
│   │   ├── search/
│   │   │   ├── components/                 # SearchForm, VenueAutocomplete,
│   │   │   │                               # EndTimePicker, SituationSelector
│   │   │   ├── hooks/useCreatePlan.ts      #   POST → planId 取得 → router.push
│   │   │   └── schema.ts                   #   zod（openapi生成型と整合させる）
│   │   ├── timeline/
│   │   │   ├── components/
│   │   │   │   ├── TimelineView.tsx        #   segments を縦に並べる器
│   │   │   │   ├── SegmentCard.tsx         #   ★type で分岐するディスパッチャ
│   │   │   │   ├── DiningSegment.tsx
│   │   │   │   ├── LodgingSegment.tsx
│   │   │   │   ├── MoveSegment.tsx
│   │   │   │   ├── BufferSegment.tsx
│   │   │   │   └── AlternativePicker.tsx   #   候補差し替えUI
│   │   │   ├── hooks/usePlanStream.ts      #   ★SSEで生成進捗を購読
│   │   │   └── utils/formatSegment.ts
│   │   └── affiliate/
│   │       └── components/AffiliateLink.tsx # rel="sponsored" + 計測を強制する唯一の出口
│   │
│   ├── components/ui/                      # Button, Card, Skeleton ...
│   ├── lib/
│   │   ├── api/{client.ts,plans.ts}        # fetchラッパ（timeout / エラー正規化）
│   │   ├── analytics/track.ts
│   │   └── utils/cn.ts
│   ├── types/api.generated.ts              # ★openapi.yaml から自動生成（手編集禁止）
│   ├── hooks/
│   └── constants/
├── public/
├── .env.local.example
├── next.config.ts
└── tsconfig.json
```

**ポイント**

- `SegmentCard.tsx` が `segment.type` で分岐する**唯一のディスパッチャ**です。後述の判別可能ユニオンにより、`switch` の網羅性チェックが型で効きます。セグメント種別を増やしたとき、コンパイルエラーが直すべき箇所を全部教えてくれます。
- **`usePlanStream.ts`（SSE）が体感速度の鍵**です。3 つの外部 API + LLM で **合計 8〜15 秒**かかるため、同期レスポンスだと確実に離脱します。`POST /v1/plans` は即座に `202 + planId` を返し、フロントは SSE で `collecting → composing → completed` の進捗を受けながら、確定したセグメントから順に描画します。
- `app/api/` は BFF に限定。厚くすると「フロント/バック完全分離」が崩れます。

---

# 2-A. リクエスト JSON — ユーザー検索条件

`POST /v1/plans` → `202 Accepted` + `{ "planId": "...", "status": "queued" }`

```jsonc
{
  "event": {
    "eventId": null,                          // イベントマスタ参照時のみ。手入力なら null
    "name": "○○ TOUR 2026 東京公演",
    "venue": {
      "name": "東京ドーム",
      "address": "東京都文京区後楽1-3-61",
      "location": { "lat": 35.7056, "lng": 139.7519 },
      "googlePlaceId": "ChIJK1kFhcSMGGARFwQMs73Kejo"   // あれば精度が上がる
    },
    "endsAt": "2026-09-12T21:00:00+09:00",    // ★全計算の起点。RFC3339・オフセット必須
    "exitBufferMinutes": 30                   // 規制退場・物販待ちの余裕
  },

  "party": {
    "adults": 2,
    "children": 0,
    "relationship": "friends"                 // "solo"|"couple"|"friends"|"family"
  },

  "situation": {                              // ★LLMの文章生成に効く「文脈」
    "intent": "stay",                         // "stay"|"return"|"either"
    "mood": ["celebrate", "relaxed"],          // "celebrate"|"relaxed"|"efficient"|"budget"
    "notes": "初めての遠征で土地勘がありません。荷物が多めです。",  // 自由記述（最大200字）
    "physicalLimit": { "maxWalkMinutes": 12 }
  },

  "dining": {
    "enabled": true,
    "genres": ["izakaya", "ramen"],
    "budgetPerPersonJpy": { "min": 2000, "max": 5000 },
    "requirements": ["open_late", "reservable", "non_smoking"],
    "desiredStayMinutes": 90
  },

  "lodging": {
    "enabled": true,
    "checkInDate": "2026-09-12",
    "checkOutDate": "2026-09-13",
    "rooms": 1,
    "budgetPerNightJpy": { "min": 0, "max": 18000 },
    "requirements": ["no_smoking", "late_checkin_ok", "with_bath"]
  },

  "returnTrip": {                             // intent が "return"/"either" のとき有効
    "destination": { "type": "station", "name": "新宿" },
    "checkLastTrain": true
  },

  "options": {
    "transportModes": ["WALK", "TRANSIT"],    // Google Routes の travelMode に対応
    "maxCandidatesPerCategory": 8,            // ★LLMに渡す候補数の上限＝コスト制御ノブ
    "llmNarrative": true                      // false なら LLM をスキップしてルールベース
  },

  "context": { "locale": "ja-JP", "currency": "JPY", "timezone": "Asia/Tokyo" }
}
```

**設計判断**

- `situation` を独立させたのが、このアプリの差別化点です。`mood` と `notes` は Go の絞り込みには使わず、**LLM のプロンプトにだけ渡します**。「初めての遠征で荷物が多い」という自由記述から「コインロッカーが近い店を先に」といった文章を生成させる余地が、機械的な API 連携との差になります。
- `maxCandidatesPerCategory` は LLM の入力トークン量を直接決めるノブです。8 件 × 3 カテゴリで入力 3〜5K トークン程度に収まり、Claude Opus 5 なら 1 リクエスト数円のオーダーです。ここをパラメータ化しておくと、後からコスト調整が設定変更だけで済みます。
- 金額は**すべて整数の JPY**。時刻は**すべて RFC3339 のオフセット付き**。日付のみが意味を持つ `checkInDate` だけ `date` 形式にしています。

---

# 2-B-1. 【中間契約】LLM に出力させる構造化 JSON

これは**フロントには出ません**。Go 内部で LLM に強制する契約です。`schemas/llm/plan_output.schema.json` として管理し、Claude なら `output_config.format`、Gemini なら `responseSchema` にそのまま渡します。

```jsonc
// LLMの出力（Composerが受け取るもの）
{
  "planTitle": "終演の余韻を、水道橋の深夜に持ち帰る",
  "planSummary": "退場の混雑を30分やり過ごしてから、深夜2時まで営業の居酒屋へ。徒歩6分のホテルに戻るまで、乗り換えは一度もありません。",
  "vibe": "relaxed_late_night",

  "steps": [
    {
      "order": 1,
      "kind": "buffer",
      "candidateId": null,                    // buffer/move は候補を持たない
      "stayMinutes": 30,
      "headline": "まずは動かず、余韻に浸る",
      "reason": "東京ドームは規制退場で最大30分かかります。急いでも進みません。",
      "tip": "この間にモバイルバッテリーの残量を確認しておくと安心です。"
    },
    {
      "order": 2,
      "kind": "move",
      "candidateId": null,
      "stayMinutes": 0,
      "headline": "水道橋方面へ徒歩で",
      "reason": "改札の混雑を避けるため、あえて一駅ぶん歩きます。",
      "tip": null
    },
    {
      "order": 3,
      "kind": "dining",
      "candidateId": "cand_dining_003",       // ★FactStore への参照のみ
      "stayMinutes": 90,
      "headline": "深夜まで開いている、地元の居酒屋へ",
      "reason": "2時までの営業なので、退場が遅れても入店できます。予算内で個室があるのはこの一軒だけでした。",
      "tip": "終演直後は混むため、歩きながらの電話予約がおすすめです。"
    },
    {
      "order": 4,
      "kind": "move",
      "candidateId": null,
      "stayMinutes": 0,
      "headline": "ホテルへ",
      "reason": "酔っていても迷わない一本道です。",
      "tip": null
    },
    {
      "order": 5,
      "kind": "lodging",
      "candidateId": "cand_hotel_001",
      "stayMinutes": 0,
      "headline": "会場徒歩圏で、翌朝もゆっくり",
      "reason": "チェックイン最終時刻が26時なので、居酒屋を出てからでも十分間に合います。",
      "tip": "翌朝は10時チェックアウト。周辺観光の余裕があります。"
    }
  ],

  "closingNote": "遠征は移動が一番疲れます。今回は「歩く距離を最小にする」ことを優先して組みました。",

  "unusedCandidateNotes": [
    { "candidateId": "cand_dining_007", "reason": "評価は高いが23時閉店で、退場時刻から逆算すると滞在30分未満になるため見送りました。" }
  ]
}
```

**この中間契約で守っているルール**

1. **`candidateId` 以外に事実を書かせない。** 店名・住所・価格・URL・営業時間は一切含みません。`cand_dining_003` が実在するかは Hydrator が FactStore で照合し、存在しなければ即エラーとして 1 回だけリトライします。
2. **絶対時刻を書かせない。** LLM に `stayMinutes`（滞在時間）だけを提案させ、絶対時刻は Go の `timecalc.go` が `endsAt` を起点に「滞在時間 + Routes API 実測の移動時間」を累積して算出します。LLM に時刻の足し算をさせると必ずズレます。
3. **`unusedCandidateNotes` を持たせる。** 「なぜ他の店ではないのか」は UI の説得力に直結し、同時に LLM が候補を吟味した証跡にもなります。

**Claude 側の呼び出し形（構造化出力）**

```jsonc
POST /v1/messages
{
  "model": "claude-opus-5",
  "max_tokens": 8000,
  "output_config": {
    "effort": "medium",                       // 選択と作文が主。medium で十分かつ速い
    "format": {
      "type": "json_schema",
      "schema": { /* 上記の構造。全 object に additionalProperties:false が必須 */ }
    }
  },
  "messages": [{ "role": "user", "content": "<prompt/templates/plan_v1.md.tmpl の展開結果>" }]
}
```

構造化出力は `additionalProperties: false` を全オブジェクトに要求し、**`minimum` / `maxLength` などの数値・長さ制約はサポートされません**。つまり「`stayMinutes` が 0〜240 の範囲か」はスキーマでは守れないので、`validator.go` 側での検査が必須になります（これが Hydrator + Validator を分離した理由でもあります）。なお新規スキーマは初回にコンパイルコストがかかり、以降 24 時間キャッシュされるため、スキーマは頻繁に変えないほうが速度面でも有利です。Gemini を使う場合も `responseMimeType: "application/json"` + `responseSchema` で同じ JSON Schema を流用できます。

---

# 2-B-2. 【最終レスポンス】Next.js へ返す UI 描画直結 JSON

`GET /v1/plans/{planId}`（SSE でも同じ `data` 形状を段階的に配信）

```jsonc
{
  "data": {
    "planId": "pln_01HX8Z9K3M",
    "status": "completed",                    // "queued"|"collecting"|"composing"|"completed"|"partial"|"failed"
    "generatedAt": "2026-08-29T19:04:11+09:00",
    "expiresAt":   "2026-08-29T19:19:11+09:00",   // ★空室情報の鮮度保証
    "shareUrl": "https://lifestyle-mapper.app/plan/pln_01HX8Z9K3M",

    // ── LLM 由来の「語り」 ─────────────────────────
    "narrative": {
      "title": "終演の余韻を、水道橋の深夜に持ち帰る",
      "summary": "退場の混雑を30分やり過ごしてから、深夜2時まで営業の居酒屋へ。徒歩6分のホテルに戻るまで、乗り換えは一度もありません。",
      "closingNote": "遠征は移動が一番疲れます。今回は「歩く距離を最小にする」ことを優先して組みました。",
      "vibe": "relaxed_late_night"
    },

    // ── Go が確定した「事実」 ───────────────────────
    "summary": {
      "startAt": "2026-09-12T21:00:00+09:00",
      "endAt":   "2026-09-13T10:00:00+09:00",
      "totalWalkMinutes": 14,
      "totalWalkMeters": 1080,
      "estimatedCostJpy": { "dining": 8000, "lodging": 16200, "transit": 0, "total": 24200 },
      "feasibility": "ok"                     // "ok"|"tight"|"infeasible"
    },

    "timeline": [
      {
        "id": "seg_1",
        "type": "buffer",
        "startAt": "2026-09-12T21:00:00+09:00",
        "endAt":   "2026-09-12T21:30:00+09:00",
        "durationMinutes": 30,
        "narrative": {
          "headline": "まずは動かず、余韻に浸る",
          "reason": "東京ドームは規制退場で最大30分かかります。急いでも進みません。",
          "tip": "この間にモバイルバッテリーの残量を確認しておくと安心です。"
        },
        "buffer": { "kind": "exit_congestion", "atPlaceName": "東京ドーム" }
      },

      {
        "id": "seg_2",
        "type": "move",
        "startAt": "2026-09-12T21:30:00+09:00",
        "endAt":   "2026-09-12T21:38:00+09:00",
        "durationMinutes": 8,
        "narrative": { "headline": "水道橋方面へ徒歩で", "reason": "改札の混雑を避けるため、あえて一駅ぶん歩きます。", "tip": null },
        "move": {                              // ★Google Routes API 由来（LLM不可侵）
          "travelMode": "WALK",
          "distanceMeters": 650,
          "durationMinutes": 8,
          "from": { "name": "東京ドーム",              "location": { "lat": 35.7056, "lng": 139.7519 } },
          "to":   { "name": "居酒屋 ○○ 水道橋店",       "location": { "lat": 35.7021, "lng": 139.7538 } },
          "encodedPolyline": "yv{xEuvfsY...",   // 地図描画用
          "mapsUrl": "https://www.google.com/maps/dir/?api=1&origin=...&destination=...",
          "transitLegs": []                     // TRANSIT時のみ: 路線名・乗車駅・発車時刻
        }
      },

      {
        "id": "seg_3",
        "type": "dining",
        "startAt": "2026-09-12T21:38:00+09:00",
        "endAt":   "2026-09-12T23:08:00+09:00",
        "durationMinutes": 90,
        "narrative": {
          "headline": "深夜まで開いている、地元の居酒屋へ",
          "reason": "2時までの営業なので、退場が遅れても入店できます。予算内で個室があるのはこの一軒だけでした。",
          "tip": "終演直後は混むため、歩きながらの電話予約がおすすめです。"
        },
        "dining": {                            // ★Google Places API 由来（LLM不可侵）
          "candidateId": "cand_dining_003",
          "providerPlaceId": "ChIJxxxxxxxxxxxx",
          "name": "居酒屋 ○○ 水道橋店",
          "genres": ["izakaya"],
          "rating": 4.1,
          "userRatingCount": 832,
          "priceLevel": 2,
          "estimatedCostPerPersonJpy": 4000,
          "address": "東京都文京区後楽1-1-1 ○○ビル2F",
          "location": { "lat": 35.7021, "lng": 139.7538 },
          "photoUrl": "https://cdn.lifestyle-mapper.app/photos/xxxx.jpg",  // Places写真をプロキシ
          "phoneNumber": "+81-3-1234-5678",
          "openingStatus": { "openNow": true, "closesAt": "2026-09-13T02:00:00+09:00" }
        },
        "links": [
          { "kind": "affiliate", "provider": "hotpepper",   "label": "ネットで予約",   "url": "https://...", "trackingId": "trk_a1" },
          { "kind": "tel",       "provider": "direct",      "label": "電話で予約",     "url": "tel:+81312345678" },
          { "kind": "map",       "provider": "google_maps", "label": "地図で見る",     "url": "https://..." }
        ]
      },

      {
        "id": "seg_5",
        "type": "lodging",
        "startAt": "2026-09-12T23:20:00+09:00",
        "endAt":   "2026-09-13T10:00:00+09:00",
        "durationMinutes": 640,
        "narrative": {
          "headline": "会場徒歩圏で、翌朝もゆっくり",
          "reason": "チェックイン最終時刻が26時なので、居酒屋を出てからでも十分間に合います。",
          "tip": "翌朝は10時チェックアウト。周辺観光の余裕があります。"
        },
        "lodging": {                           // ★楽天トラベルAPI 由来（LLM不可侵）
          "candidateId": "cand_hotel_001",
          "providerHotelId": "123456",
          "name": "○○ホテル 後楽園",
          "reviewAverage": 4.2,
          "reviewCount": 1204,
          "address": "東京都文京区春日1-1-1",
          "location": { "lat": 35.7075, "lng": 139.7519 },
          "photoUrl": "https://img.travel.rakuten.co.jp/.../xxxx.jpg",
          "plan": {
            "providerPlanId": "9876543",
            "planName": "【素泊まり】直前割・チェックイン26時までOK",
            "roomName": "セミダブル 禁煙",
            "totalPriceJpy": 16200,
            "pricePerPersonJpy": 8100,
            "vacancyStatus": "few_left",       // "available"|"few_left"|"sold_out"
            "remainingRooms": 2,
            "checkInTime": "15:00",
            "checkInDeadline": "26:00",
            "checkOutTime": "10:00",
            "amenities": ["free_wifi", "large_bath", "coin_laundry"]
          }
        },
        "links": [
          { "kind": "affiliate", "provider": "rakuten_travel", "label": "楽天トラベルで予約",
            "url": "https://hb.afl.rakuten.co.jp/hgc/xxxxxxxx/?pc=https%3A%2F%2Ftravel.rakuten.co.jp%2F...",
            "trackingId": "trk_b7" }
        ]
      }
    ],

    // ── 差し替え候補（UIのAlternativePickerが使用） ──────
    "alternatives": {
      "dining":  [ { "candidateId": "cand_dining_007", /* dining と同形 */
                     "excludedReason": "23時閉店のため滞在30分未満になります" } ],
      "lodging": [ { "candidateId": "cand_hotel_004",  /* lodging と同形 */ "excludedReason": null } ]
    },

    "warnings": [
      { "code": "VACANCY_LOW", "severity": "info", "segmentId": "seg_5",
        "message": "このプランの残室は2室です。お早めのご予約をおすすめします。" },
      { "code": "LAST_TRAIN_MISSED", "severity": "warning", "segmentId": null,
        "message": "新宿方面の終電（23:52 水道橋発）には間に合いません。宿泊プランを提案しています。" }
    ]
  },

  "meta": {
    "sources": [                               // ★並行fan-outの部分失敗を正直に返す
      { "provider": "google_places",  "status": "ok",       "latencyMs": 412,  "cached": false, "resultCount": 24 },
      { "provider": "google_routes",  "status": "ok",       "latencyMs": 388,  "cached": true,  "resultCount": 24 },
      { "provider": "rakuten_travel", "status": "degraded", "latencyMs": 3000, "cached": false, "resultCount": 0,
        "error": { "code": "UPSTREAM_TIMEOUT", "message": "楽天トラベルAPIの応答がありません" } }
    ],
    "llm": {
      "provider": "anthropic", "model": "claude-opus-5",
      "schemaVersion": "plan_output.v1", "promptVersion": "plan_v1",
      "latencyMs": 4820, "inputTokens": 3204, "outputTokens": 1180,
      "repairAttempts": 0                      // Validator不合格による再生成回数
    },
    "requestId": "req_01HX8Z9K3M"
  }
}
```

**エラー時**

```jsonc
{ "error": {
    "code": "VALIDATION_FAILED",
    "message": "検索条件が不正です",
    "details": [ { "field": "event.endsAt", "reason": "過去の日時は指定できません" } ],
    "requestId": "req_01HX8Z9K3M" } }
```

---

## 最終スキーマで意図的にそうした点

1. **`narrative`（LLM 由来）と事実フィールドを、全階層で物理的に分離。**
   セグメント直下も `narrative` と `dining` / `lodging` / `move` が並びます。これにより「LLM が触れてよい場所」がスキーマ上で自明になり、レビューでも「`narrative` の外に LLM の文字列が入っていないか」だけを見れば済みます。UI 側も、LLM を停止したい日は `narrative` を無視すれば機能を落とさず動きます。

2. **`timeline` は `type` による判別可能ユニオン。**
   Go は各詳細をポインタ + `omitempty`、TypeScript は discriminated union になります。

   ```go
   type Segment struct {
       ID              string         `json:"id"`
       Type            SegmentType    `json:"type"`
       StartAt         time.Time      `json:"startAt"`
       EndAt           time.Time      `json:"endAt"`
       DurationMinutes int            `json:"durationMinutes"`
       Narrative       *Narrative     `json:"narrative,omitempty"`  // LLM由来（nil可）
       Buffer          *BufferDetail  `json:"buffer,omitempty"`
       Move            *MoveDetail    `json:"move,omitempty"`
       Dining          *DiningDetail  `json:"dining,omitempty"`
       Lodging         *LodgingDetail `json:"lodging,omitempty"`
       Links           []Link         `json:"links,omitempty"`
   }
   ```
   ```ts
   type SegmentBase = { id: string; startAt: string; endAt: string;
                        durationMinutes: number; narrative: Narrative | null };
   type Segment = SegmentBase & (
     | { type: "buffer";  buffer: BufferDetail;  links?: never }
     | { type: "move";    move: MoveDetail;      links?: never }
     | { type: "dining";  dining: DiningDetail;  links: Link[] }
     | { type: "lodging"; lodging: LodgingDetail; links: Link[] }
   );
   ```

3. **`status` を最初から持たせ、SSE と同形にした。**
   `POST` が `202 + planId` を返し、SSE が同じ `data` を `status` を進めながら送るため、フロントは「初回描画」と「ストリーム更新」で別の型を扱う必要がありません。同期実装から非同期実装への移行もスキーマ変更なしで済みます。

4. **`meta.sources` と `meta.llm` を必ず返す。**
   Goroutine 並行処理では部分失敗が日常的に起きます。楽天が落ちても飲食店だけ返して `status: "partial"` + `warnings` を出すほうが、全体 500 より圧倒的に良い UX です。`meta.llm.repairAttempts` は Validator の不合格率＝プロンプト品質の実測値になるので、CloudWatch に流しておくと改善サイクルが回せます。

5. **`links[]` として構造化し、`trackingId` を発行。**
   アフィリエイト URL を素の文字列で置くと、ASP 追加時に全画面改修になります。`kind` / `provider` / `trackingId` を持たせておけば、フロントは `AffiliateLink` コンポーネント 1 つで `rel="sponsored"` とクリック計測（`POST /v1/clicks`）を一括で担保できます。**収益がかかっている箇所なので、抜け道のない単一の出口を作るのが重要**です。

6. **`expiresAt` を明示。**
   ホテルの空室は数分で変わります。期限切れをフロントが検知して再検索を促すことで、「クリックしたら満室だった」という最も痛い離脱を防げます。

---

## 実装の優先順位（提案）

1. `schemas/openapi.yaml` + `schemas/llm/plan_output.schema.json` を書き、TS 型生成を通す
2. Go の `collector.go`（errgroup での 3API 並行 + 部分失敗許容）— ここが Go 採用の本丸
3. `factstore.go` → `hydrator.go` → `validator.go`（**LLM より先に**。LLM なしでも時系列は組める状態を作る）
4. `composer.go` で LLM を接続（`options.llmNarrative: false` でいつでも切り離せる形を維持）
5. SSE と Terraform

---

次のステップとして、`schemas/` の実ファイル起こし、`collector.go` の errgroup 実装、`docker-compose.yml` でのローカル環境構築のいずれからでも着手できます。どれから進めましょうか。

なお、`go` が PATH に見つからないため、バックエンド着手前に Go のインストール状況をご確認ください。
