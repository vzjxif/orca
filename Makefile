.PHONY: build frontend test dev

frontend:
	cd frontend && npm ci && npm run build

build: frontend
	rm -rf backend/web/dist
	mkdir -p backend/web/dist
	cp -R frontend/dist/. backend/web/dist/
	cd backend && go build -trimpath -o orca .

test:
	cd backend && go test ./...
	cd frontend && npm run build

dev:
	cd backend && go run .
