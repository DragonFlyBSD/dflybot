NAME=	urlshort

.PHONY: all
all: $(NAME)

.PHONY: $(NAME)
$(NAME):
	go mod tidy
	go vet ./...
	env CGO_ENABLED=0 \
		go build -o $(NAME) -trimpath
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o $(NAME).linux -trimpath
	env CGO_ENABLED=0 GOOS=dragonfly GOARCH=amd64 \
		go build -o $(NAME).dragonfly -trimpath

.PHONY: test
test:
	go vet ./...
	go test ./...

.PHONY: bench
bench:
	go test -run '^$$' -bench . -benchmem ./...

.PHONY: clean
clean:
	rm -f $(NAME) $(NAME).linux $(NAME).dragonfly
