package llm

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// canonicalSchemaPath は契約の正本。埋め込みファイルはこれの複製にすぎない。
const canonicalSchemaPath = "../../../../schemas/llm/plan_output.schema.json"

// 複製が正本からずれたまま本番に出ると、LLM に渡すスキーマとフロントの期待が食い違う。
// go:embed がディレクトリを跨げない以上、この検査が唯一の防波堤になる。
func TestEmbeddedSchemaMatchesCanonical(t *testing.T) {
	want, err := os.ReadFile(canonicalSchemaPath)
	if err != nil {
		t.Fatalf("正本を読めません: %v", err)
	}
	if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(planOutputSchema)) {
		t.Error("埋め込みスキーマが schemas/llm/plan_output.schema.json とずれています。" +
			"`make sync-llm-schema` を実行してください")
	}
}

func TestSchemaVersionMatchesID(t *testing.T) {
	var doc struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(planOutputSchema, &doc); err != nil {
		t.Fatalf("スキーマを解釈できません: %v", err)
	}
	// 例: https://lifestyle-mapper.app/schemas/llm/plan_output.v1.json
	base := doc.ID[strings.LastIndex(doc.ID, "/")+1:]
	if got := strings.TrimSuffix(base, ".json"); got != SchemaVersion {
		t.Errorf("SchemaVersion = %q ですが、スキーマの $id は %q です", SchemaVersion, got)
	}
}

// 構造化出力は全 object に additionalProperties:false を要求する。
// 1 つでも欠けると Claude 側が strict mode を受け付けない。
func TestSchemaIsStrictModeCompatible(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(planOutputSchema, &doc); err != nil {
		t.Fatalf("スキーマを解釈できません: %v", err)
	}
	var walk func(node any, path string)
	walk = func(node any, path string) {
		m, ok := node.(map[string]any)
		if !ok {
			return
		}
		if m["type"] == "object" {
			if v, ok := m["additionalProperties"].(bool); !ok || v {
				t.Errorf("%s: additionalProperties:false がありません", path)
			}
			if _, ok := m["required"]; !ok {
				t.Errorf("%s: required がありません（strict mode は全項目必須）", path)
			}
		}
		for k, v := range m {
			switch k {
			case "properties":
				if props, ok := v.(map[string]any); ok {
					for name, p := range props {
						walk(p, path+"."+name)
					}
				}
			case "items":
				walk(v, path+"[]")
			}
		}
	}
	walk(doc, "$")
}

func TestSchemaReturnsCopy(t *testing.T) {
	s := Schema()
	if len(s) == 0 {
		t.Fatal("スキーマが空です")
	}
	s[0] = 'X'
	if planOutputSchema[0] == 'X' {
		t.Error("Schema() の戻り値経由で埋め込みスキーマが書き換えられます")
	}
	// 明示指定があればそちらを優先する。
	custom := []byte(`{"type":"object"}`)
	if !bytes.Equal(schemaFor(Request{Schema: custom}), custom) {
		t.Error("Request.Schema が優先されていません")
	}
	if !bytes.Equal(schemaFor(Request{}), planOutputSchema) {
		t.Error("未指定時に埋め込みスキーマが使われていません")
	}
}
