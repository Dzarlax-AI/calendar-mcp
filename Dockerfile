FROM node:22-alpine AS frontend-builder
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/index.html frontend/tsconfig*.json frontend/vite.config.ts ./
COPY frontend/src ./src
RUN npm run build

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend-builder /src/frontend/dist ./frontend/dist
RUN mkdir -p internal/web/spa/dist && cp -R frontend/dist/. internal/web/spa/dist/
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /app/calendar ./cmd/calendar

FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/calendar .

RUN mkdir -p /app/data

EXPOSE 8080

ENTRYPOINT ["/app/calendar"]
CMD ["serve"]
