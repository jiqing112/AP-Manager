# apmanager 构建入口。css 先于 build（embed 依赖 dist/app.css 存在）

BINARY := bin/apmanager
TW     := ./bin/tailwindcss

.PHONY: css css-watch build dev release check clean

css: ## 生成 web/dist/app.css（minify）
	$(TW) -c web/tailwind.config.js -i web/src/input.css -o web/dist/app.css --minify

css-watch: ## 监听模式（开发）
	$(TW) -c web/tailwind.config.js -i web/src/input.css -o web/dist/app.css --watch

build: css ## 完整构建（CSS + Go）
	@test -s web/dist/app.css || { echo "错误：web/dist/app.css 为空，先跑 make css"; exit 1; }
	go build -trimpath -ldflags "-s -w" -o $(BINARY) .

dev: css ## UI 开发模式（假数据，不动系统）
	go run . --dev -state ./var -log ./var/app.log

release: css ## 交叉编译产物（当前机即目标机，保留备用目标）
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o $(BINARY) .

check: ## vet + embed 资源完整性
	go vet ./...
	@test -s web/dist/app.css && echo "dist/app.css OK"
	@test -s web/vendor/alpine.min.js && echo "alpine OK"
	@test -s web/vendor/chart.umd.js && echo "chart.js OK"

run: build ## 本机真跑（root；监听 127.0.0.1:8080）
	sudo $(BINARY)

clean:
	rm -rf bin/apmanager web/dist/app.css
