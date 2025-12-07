.PHONY: build test coverage clean help

# Build the project
build:
	go build -v ./...

# Run tests
test:
	go test -v ./...

# Run tests with coverage
coverage:
	go test -v -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report generated: coverage.html"
	go tool cover -func=coverage.out

# Run tests with coverage and open in browser
coverage-html: coverage
	@command -v open >/dev/null 2>&1 && open coverage.html || \
	(command -v xdg-open >/dev/null 2>&1 && xdg-open coverage.html || \
	echo "Please open coverage.html manually")

# Clean build artifacts and coverage files
clean:
	go clean
	rm -f coverage.out coverage.html

# Show help
help:
	@echo "Available targets:"
	@echo "  build         - Build the project"
	@echo "  test          - Run tests"
	@echo "  coverage      - Run tests and generate coverage report"
	@echo "  coverage-html - Run coverage and open HTML report in browser"
	@echo "  clean         - Clean build artifacts and coverage files"
	@echo "  help          - Show this help message"
