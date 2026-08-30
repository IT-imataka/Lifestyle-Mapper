#!/usr/bin/env python3
"""LLM 構造化出力スキーマが strict mode の制約を満たすか検査する。

Claude の output_config.format / Gemini の responseSchema は、
  - 全 object に additionalProperties: false が必要
  - 全 property が required に列挙されている必要がある（nullable は type: [T, "null"] で表現）
  - minimum / maximum / maxLength / pattern / format などの値制約は無視される
という制約を持つ。3 番目が特に危険で、「スキーマに書いたから守られる」と誤解すると
validator.go の検査が漏れる。ここで CI 落としておく。
"""
import json
import sys
from pathlib import Path

SCHEMA_PATH = Path(__file__).resolve().parent.parent / "schemas" / "llm" / "plan_output.schema.json"

# 構造化出力では効かない（＝Go 側 validator で担保すべき）キーワード
IGNORED_KEYWORDS = {
    "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
    "minLength", "maxLength", "pattern", "format",
    "minItems", "maxItems", "uniqueItems",
    "minProperties", "maxProperties",
    "allOf", "not", "if", "then", "else",
}

errors: list[str] = []
warnings: list[str] = []


def walk(node: object, path: str) -> None:
    if isinstance(node, list):
        for i, item in enumerate(node):
            walk(item, f"{path}[{i}]")
        return
    if not isinstance(node, dict):
        return

    node_types = node.get("type")
    types = node_types if isinstance(node_types, list) else [node_types]

    if "object" in types:
        if node.get("additionalProperties") is not False:
            errors.append(f"{path}: object に additionalProperties: false がない")
        props = node.get("properties", {})
        required = set(node.get("required", []))
        missing = sorted(set(props) - required)
        if missing:
            errors.append(
                f"{path}: 全 property を required に入れる必要がある（未記載: {', '.join(missing)}）"
            )
        extra = sorted(required - set(props))
        if extra:
            errors.append(f"{path}: required に存在しない property がある（{', '.join(extra)}）")

    for kw in sorted(IGNORED_KEYWORDS & node.keys()):
        warnings.append(f"{path}: `{kw}` は構造化出力では無視される。validator.go で検査すること")

    if "description" not in node and path != "$":
        # 説明のないフィールドは LLM が意図を推測できず品質が落ちる
        if node.get("type") is not None or "$ref" in node:
            warnings.append(f"{path}: description がない（LLM への指示が不足）")

    for key in ("properties", "$defs", "definitions"):
        for name, sub in node.get(key, {}).items():
            walk(sub, f"{path}.{name}")
    if "items" in node:
        walk(node["items"], f"{path}[]")
    for key in ("oneOf", "anyOf"):
        walk(node.get(key, []), f"{path}.{key}")


def main() -> int:
    schema = json.loads(SCHEMA_PATH.read_text(encoding="utf-8"))
    walk(schema, "$")

    for w in warnings:
        print(f"warn  {w}")
    for e in errors:
        print(f"ERROR {e}")

    if errors:
        print(f"\n{SCHEMA_PATH.name}: {len(errors)} 件の違反")
        return 1
    print(f"\n{SCHEMA_PATH.name}: strict mode 適合（warn {len(warnings)} 件）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
