FROM golang:1.22-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o loto-backend main.go

FROM alpine:3.20

WORKDIR /app

COPY --from=builder /app/loto-backend ./loto-backend

EXPOSE 4000
CMD ["./loto-backend"]
