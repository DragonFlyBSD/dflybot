NAME=	dflybot

.PHONY: all
all: dflybot
all: git-monitor
all: github-monitor
all: jenkins-monitor
all: web-monitor

.PHONY: dflybot
dflybot:
	go mod tidy
	go vet ./...
	env CGO_ENABLED=0 \
		go build -o $(NAME) -trimpath
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o $(NAME).linux -trimpath
	env CGO_ENABLED=0 GOOS=dragonfly GOARCH=amd64 \
		go build -o $(NAME).dragonfly -trimpath

.PHONY: git-monitor
git-monitor:
	make -C git-monitor

.PHONY: github-monitor
github-monitor:
	make -C github-monitor

.PHONY: jenkins-monitor
jenkins-monitor:
	make -C jenkins-monitor

.PHONY: web-monitor
web-monitor:
	make -C web-monitor
