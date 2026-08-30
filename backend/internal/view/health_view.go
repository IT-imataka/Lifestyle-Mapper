package view

// Health は GET /health の応答。
//
// 外枠（{data, meta}）に載せない唯一の応答。ALB / ECS のヘルスチェックが読むため、
// 余計な階層を挟まず最小の JSON にしている。
type Health struct {
	Status  string `json:"status"`
	Version string `json:"version,omitempty"`
}

// HealthOK は openapi が const: ok と宣言している値。
const HealthOK = "ok"

func NewHealth(version string) Health {
	return Health{Status: HealthOK, Version: version}
}
