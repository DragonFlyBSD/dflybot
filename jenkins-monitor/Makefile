NAME=	jenkins-monitor

.PHONY: all
all:
	go mod tidy
	go vet ./...
	env CGO_ENABLED=0 \
		go build -o $(NAME) -trimpath
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o $(NAME).linux -trimpath
	env CGO_ENABLED=0 GOOS=dragonfly GOARCH=amd64 \
		go build -o $(NAME).dragonfly -trimpath
