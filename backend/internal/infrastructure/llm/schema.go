package llm

import (
	_ "embed"
)

// SchemaVersion は meta.llm.schemaVersion に載る版。埋め込みスキーマの $id と一致する
// （schema_test.go が乖離を検出する）。
//
// 構造化出力のスキーマは初回にコンパイルコストがかかり、以降 24 時間キャッシュされる。
// 頻繁に変えると速度面で不利なので、版を上げるのは形が本当に変わるときだけにする。
const SchemaVersion = "plan_output.v1"

// planOutputSchema は LLM に強制する構造化出力スキーマ。
//
// 正本は schemas/llm/plan_output.schema.json で、ここにあるのはビルド用の複製。
// go:embed はパッケージディレクトリの外を参照できないため複製せざるを得ない。
// `make sync-llm-schema` が複製を更新し、schema_test.go が乖離をビルド前に落とす。
//
//go:embed plan_output.schema.json
var planOutputSchema []byte

// Schema は構造化出力に渡す JSON Schema を返す。
// Claude なら output_config.format.schema、Gemini なら responseSchema にそのまま入る。
func Schema() []byte {
	// 呼び出し側に書き換えられないよう複製を返す。
	out := make([]byte, len(planOutputSchema))
	copy(out, planOutputSchema)
	return out
}

// schemaFor はリクエストで指定が無ければ埋め込みスキーマを使う。
func schemaFor(req Request) []byte {
	if len(req.Schema) > 0 {
		return req.Schema
	}
	return planOutputSchema
}
