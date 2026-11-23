FROM node:22-alpine AS frontend
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-alpine AS backend
ENV GOPROXY=https://goproxy.io,direct
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
COPY --from=frontend /src/frontend/dist/ ./web/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/orca .

FROM docker:28-cli AS runtime
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=backend /out/orca /usr/local/bin/orca
ENV ORCA_DATA_DIR=/var/lib/orca ORCA_BACKUP_DIR=/backups
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/orca"]
