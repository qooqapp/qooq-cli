BIN := bin

# 版本從 git 來，不是寫死的。--dirty 讓「工作區有未提交改動」的 build 一眼看得出來。
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)

.PHONY: all build vet test check clean version

all: build

## build: 編譯 qooq CLI 到 bin/qooq（需要 qooq repo checkout 在 ../qooq，go.mod 有 replace）
build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN)/qooq ./cmd/qooq

## version: 印出這次 build 會嵌入的版本
version:
	@echo "$(VERSION)"

vet:
	go vet ./...

test:
	go test ./...

## check: vet + 依賴牆自檢——CLI 不得沾 qooqd/watcher 的重量級生態（gin/fx/gorm/go-git/docker SDK）
check: vet
	@echo "check: 所有 Go 檔都 gofmt 過"
	@# 這不是品味問題，是「diff 裡只出現真的改動」的前提：一個沒對齊的 struct tag
	@# 會讓下一個加欄位的人順手重排，於是那次的 diff 混進十行無關的空白改動，
	@# 而 review 的人得自己分辨哪幾行是真的。2026-10-15 就有兩個檔長期沒對齊，
	@# 因為在這之前沒有任何東西會講。
	@# 範圍走 go list 而不是 `.`：後者會掃到 .shapes-cache 之類的第三方快取
	@# （go 工具本來就跳過 `.` 開頭的目錄，這裡借用同一條規則）。
	@out=$$(gofmt -l $$(go list -f '{{.Dir}}' ./...)); \
	  if [ -n "$$out" ]; then \
	    echo "  這些檔沒 gofmt 過（用 gofmt -w 修）："; \
	    echo "$$out" | sed 's/^/    /'; \
	    exit 1; \
	  fi
	@echo "check: qooq CLI 不得沾 gin/fx/gorm/go-git/docker SDK"
	@! go list -deps ./cmd/qooq | grep -E 'gin-gonic|uber.org/fx|gorm|go-git|docker/docker'
	@echo "check: OK"

clean:
	rm -rf $(BIN)
	go clean
