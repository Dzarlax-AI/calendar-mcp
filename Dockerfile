FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-w -s" -o /app/calendar ./cmd/calendar

FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=builder /app/calendar .

RUN mkdir -p /app/data

EXPOSE 8080

ENTRYPOINT ["/app/calendar"]
CMD ["serve"]
