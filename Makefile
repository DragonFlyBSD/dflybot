NAME=	dflybot

.PHONY: all
all: dflybot
all: git-monitor
all: github-monitor
all: jenkins-monitor
all: redmine-monitor
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
	make -C $@

.PHONY: github-monitor
github-monitor:
	make -C $@

.PHONY: jenkins-monitor
jenkins-monitor:
	make -C $@

.PHONY: redmine-monitor
redmine-monitor:
	make -C $@

.PHONY: web-monitor
web-monitor:
	make -C $@
