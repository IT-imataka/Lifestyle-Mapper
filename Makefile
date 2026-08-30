# Lifestyle Mapper — モノレポのタスクランナー
# 契約（schemas/）を起点に、フロント型生成とバックエンド検証を回す。

SHELL := /bin/bash
.DEFAULT_GOAL := help

FRONTEND := frontend
BACKEND  := backend
SCHEMAS  := schemas

.PHONY: help
help: ## このヘルプを表示する
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ── 契約 ────────────────────────────────────────────────
.PHONY: schemas
schemas: schema-check gen-types sync-llm-schema ## 契約を検証し、フロントの型と Go の埋め込みを再生成する

.PHONY: schema-check
schema-check: ## LLM スキーマが構造化出力の strict mode に適合するか検査する
	@python3 scripts/check_llm_schema.py

.PHONY: gen-types
gen-types: ## openapi.yaml から TypeScript 型を生成する
	@cd $(FRONTEND) && npm run gen:api

.PHONY: sync-llm-schema
sync-llm-schema: ## LLM スキーマを Go の go:embed 用にコピーする
	@cp $(SCHEMAS)/llm/plan_output.schema.json \
	   $(BACKEND)/internal/infrastructure/llm/plan_output.schema.json
	@echo "  LLM スキーマを $(BACKEND)/internal/infrastructure/llm/ へ同期しました"

# ── フロントエンド ──────────────────────────────────────
.PHONY: fe-install
fe-install: ## フロントの依存を入れる
	@cd $(FRONTEND) && npm ci

.PHONY: fe-dev
fe-dev: ## Next.js を開発モードで起動する
	@cd $(FRONTEND) && npm run dev

.PHONY: fe-check
fe-check: ## フロントの型検査と Lint
	@cd $(FRONTEND) && npm run typecheck && npm run lint

# ── バックエンド ────────────────────────────────────────
.PHONY: be-run
be-run: ## Go API サーバを起動する
	@cd $(BACKEND) && go run ./cmd/api

.PHONY: be-test
be-test: ## Go のテストを実行する（外部 API は testdata で代替）
	@cd $(BACKEND) && go test ./... -race -count=1

.PHONY: be-check
be-check: ## Go の vet と build
	@cd $(BACKEND) && go vet ./... && go build ./...

.PHONY: be-tidy
be-tidy: ## go.mod を整理する
	@cd $(BACKEND) && go mod tidy

# ── まとめ ──────────────────────────────────────────────
.PHONY: check
check: schema-check be-check be-test fe-check ## CI で回す一式

.PHONY: up
up: ## postgres / redis をローカル起動する
	@docker compose up -d

.PHONY: down
down: ## ローカル環境を落とす
	@docker compose down
