build:
	@go build -o bin/ssmbak cmd/main.go

run: build
	@./bin/ssmbak

clean:
	@rm -f bin/ssmbak