package controller

import (
	"net/http"

	"github.com/taka/lifestyle-mapper/backend/internal/view"
)

// HealthController は GET /health。
//
// ALB と ECS が数秒ごとに叩くため、**依存を一切見ない**。
// DB や Redis の疎通をここで確認すると、キャッシュが落ちただけでタスクが
// 入れ替え続けられ、動いていたはずの機能まで巻き添えで止まる。
type HealthController struct {
	version string
}

func NewHealthController(version string) *HealthController {
	return &HealthController{version: version}
}

func (c *HealthController) Get(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, view.NewHealth(c.version))
}
